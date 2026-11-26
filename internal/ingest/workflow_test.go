package ingest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

const testModelTaskQueue = "trelis-models-test"

// batchCall is one EmbedBatch attempt: its seq range and the queue it was
// scheduled on.
type batchCall struct {
	From, To int
	Queue    string
}

// stubActivities stands in for the service-backed activities in workflow
// tests. Each call is recorded, and the queue an activity was scheduled on
// comes from its activity context, which is how the tests see that the embed
// batches run on the model queue.
type stubActivities struct {
	mu sync.Mutex
	// chunks is what ParseAndChunk reports and therefore what Run fans out.
	chunks     int
	parseCalls int
	// batchFailures is how many times each range fails before succeeding.
	batchFailures map[[2]int]int
	// batchErr, when set, fails every batch with it, retry rules aside.
	batchErr      error
	batchCalls    []batchCall
	completeCalls int
	completeErr   error
	// failedReasons are the reasons passed to MarkFailed.
	failedReasons []string
	// events records activity starts in order, so a test can see that complete
	// ran after the batches.
	events []string
}

func (s *stubActivities) parseAndChunk(context.Context, string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parseCalls++
	s.events = append(s.events, "parse")
	return s.chunks, nil
}

func (s *stubActivities) embedBatch(ctx context.Context, _ string, from, to int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchCalls = append(s.batchCalls, batchCall{From: from, To: to, Queue: activity.GetInfo(ctx).TaskQueue})
	s.events = append(s.events, "batch")
	if s.batchErr != nil {
		return 0, s.batchErr
	}
	key := [2]int{from, to}
	if s.batchFailures[key] > 0 {
		s.batchFailures[key]--
		return 0, errors.New("embedding provider timed out")
	}
	return to - from, nil
}

func (s *stubActivities) complete(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls++
	s.events = append(s.events, "complete")
	return s.completeErr
}

func (s *stubActivities) markFailed(_ context.Context, _, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failedReasons = append(s.failedReasons, reason)
	s.events = append(s.events, "mark_failed")
	return nil
}

// snapshot returns copies of the recorded calls under the lock.
func (s *stubActivities) snapshot() (parse, complete int, batches []batchCall, failed, events []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parseCalls, s.completeCalls, append([]batchCall(nil), s.batchCalls...),
		append([]string(nil), s.failedReasons...), append([]string(nil), s.events...)
}

// newWorkflowEnv registers the stubs under the names the workflow calls and
// returns the test environment.
func newWorkflowEnv(t *testing.T, stubs *stubActivities) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(stubs.parseAndChunk, activity.RegisterOptions{Name: ParseAndChunkActivity})
	env.RegisterActivityWithOptions(stubs.embedBatch, activity.RegisterOptions{Name: EmbedBatchActivity})
	env.RegisterActivityWithOptions(stubs.complete, activity.RegisterOptions{Name: CompleteActivity})
	env.RegisterActivityWithOptions(stubs.markFailed, activity.RegisterOptions{Name: MarkFailedActivity})
	return env
}

// runWorkflow executes the workflow with chunks Chunks and the test batch
// size.
func runWorkflow(t *testing.T, stubs *stubActivities, documentID string) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := newWorkflowEnv(t, stubs)
	workflowDef := &IngestWorkflow{ModelTaskQueue: testModelTaskQueue, EmbedBatchSize: 3}
	env.ExecuteWorkflow(workflowDef.Run, documentID)
	return env
}

func TestIngestWorkflowEmbedsInBatchesOnTheModelQueue(t *testing.T) {
	stubs := &stubActivities{chunks: 7}
	env := runWorkflow(t, stubs, "b1c2d3e4-0000-0000-0000-000000000001")
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v, want success", err)
	}

	parse, complete, batches, failed, events := stubs.snapshot()
	if parse != 1 || complete != 1 {
		t.Errorf("parse and complete calls = %d and %d, want one each", parse, complete)
	}
	want := [][2]int{{0, 3}, {3, 6}, {6, 7}}
	if len(batches) != len(want) {
		t.Fatalf("embed batches = %+v, want ranges %v", batches, want)
	}
	// Batches finish in no fixed order, so match them as a set.
	got := map[[2]int]string{}
	for _, batch := range batches {
		got[[2]int{batch.From, batch.To}] = batch.Queue
	}
	for _, wantRange := range want {
		if queue, ok := got[wantRange]; !ok || queue != testModelTaskQueue {
			t.Errorf("batch %v ran on %q, want %q", wantRange, queue, testModelTaskQueue)
		}
	}
	if len(failed) != 0 {
		t.Errorf("MarkFailed called with %q, want never on success", failed)
	}
	if len(events) == 0 || events[0] != "parse" || events[len(events)-1] != "complete" {
		t.Errorf("activity events = %v, want parse first and complete last", events)
	}
}

func TestIngestWorkflowRetriesOneBatchAlone(t *testing.T) {
	stubs := &stubActivities{chunks: 7, batchFailures: map[[2]int]int{{3, 6}: 1}}
	env := runWorkflow(t, stubs, "b1c2d3e4-0000-0000-0000-000000000002")
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v, want the retried batch to succeed", err)
	}

	parse, complete, batches, failed, _ := stubs.snapshot()
	if parse != 1 {
		t.Errorf("ParseAndChunk ran %d times, want once: a failed batch must not re-parse", parse)
	}
	if complete != 1 {
		t.Errorf("Complete ran %d times, want once", complete)
	}
	counts := map[[2]int]int{}
	for _, batch := range batches {
		counts[[2]int{batch.From, batch.To}]++
		if batch.Queue != testModelTaskQueue {
			t.Errorf("batch %+v ran on %q, want %q", batch, batch.Queue, testModelTaskQueue)
		}
	}
	want := map[[2]int]int{{0, 3}: 1, {3, 6}: 2, {6, 7}: 1}
	for key, n := range want {
		if counts[key] != n {
			t.Errorf("batch %v ran %d times, want %d", key, counts[key], n)
		}
	}
	if len(failed) != 0 {
		t.Errorf("MarkFailed called with %q, want never after a successful retry", failed)
	}
}

func TestIngestWorkflowMarksAnUnconfiguredEmbeddingFailedOnce(t *testing.T) {
	reason := "no embedding model is configured"
	stubs := &stubActivities{chunks: 7, batchErr: temporal.NewNonRetryableApplicationError(
		reason, embeddingUnconfiguredType, nil)}
	env := runWorkflow(t, stubs, "b1c2d3e4-0000-0000-0000-000000000003")
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("workflow succeeded, want the unconfigured batches to fail it")
	}

	parse, complete, batches, failed, _ := stubs.snapshot()
	if parse != 1 || complete != 0 {
		t.Errorf("parse and complete calls = %d and %d, want parse once and no complete", parse, complete)
	}
	if len(batches) != 3 {
		t.Errorf("embed batches = %+v, want all three attempted", batches)
	}
	if len(failed) != 1 || failed[0] != reason {
		t.Errorf("MarkFailed reasons = %q, want exactly [%q]", failed, reason)
	}
}

func TestIngestWorkflowDoesNotMarkFailedWhenTheDocumentIsGone(t *testing.T) {
	stubs := &stubActivities{chunks: 2, completeErr: temporal.NewNonRetryableApplicationError(
		"the Document does not exist", documentNotFoundType, nil)}
	env := runWorkflow(t, stubs, "b1c2d3e4-0000-0000-0000-000000000004")
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("workflow succeeded, want the missing Document to fail it")
	}

	parse, complete, _, failed, _ := stubs.snapshot()
	if parse != 1 || complete != 1 {
		t.Errorf("parse and complete calls = %d and %d, want one each", parse, complete)
	}
	if len(failed) != 0 {
		t.Errorf("MarkFailed called with %q, want nothing marked when the Document is gone", failed)
	}
}

func TestFailureReasonOnlyShowsMessagesChosenForReaders(t *testing.T) {
	// A retried storage or database failure arrives as an application error
	// whose message is the raw Go error, which can name an endpoint, bucket
	// or key. It must not become the Document's parse_error.
	internal := temporal.NewApplicationError(
		`put object: https://silo.internal:9000/private-bucket/documents/1/source: access denied`, "Error")
	if got := failureReason(internal); got != "the Document could not be ingested" {
		t.Errorf("failureReason(internal error) = %q, want the generic sentence", got)
	}

	unparseable := temporal.NewNonRetryableApplicationError(
		"the Source File could not be parsed", unparseableDocumentType, nil)
	if got := failureReason(unparseable); got != "the Source File could not be parsed" {
		t.Errorf("failureReason(unparseable) = %q, want the parser's safe reason", got)
	}

	unconfigured := temporal.NewNonRetryableApplicationError(
		"no embedding model is configured", embeddingUnconfiguredType, nil)
	if got := failureReason(unconfigured); got != "no embedding model is configured" {
		t.Errorf("failureReason(unconfigured embedding) = %q, want its safe reason", got)
	}

	// The same types with no message fall back rather than storing "".
	for _, typ := range []string{unparseableDocumentType, embeddingUnconfiguredType} {
		empty := temporal.NewNonRetryableApplicationError("", typ, nil)
		if got := failureReason(empty); got != "the Document could not be ingested" {
			t.Errorf("failureReason(empty %s message) = %q, want the generic sentence", typ, got)
		}
	}
}
