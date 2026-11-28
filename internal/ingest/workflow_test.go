package ingest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

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
	// parseErr, when set, fails ParseAndChunk with it.
	parseErr error
	// parseGate, when non-nil, holds ParseAndChunk until it is closed, so a
	// test can cancel the workflow while parse is in flight.
	parseGate chan struct{}
	// batchesGate, when non-nil, holds every embed batch until it is closed,
	// so a test can cancel the workflow while a batch is in flight.
	batchesGate chan struct{}
	// batchFailures is how many times each range fails before succeeding.
	batchFailures map[[2]int]int
	// batchFailureErr, when set, is what batchFailures returns instead of the
	// generic retryable error.
	batchFailureErr error
	// batchErr, when set, fails every batch with it, retry rules aside.
	batchErr      error
	batchCalls    []batchCall
	completeCalls int
	completeErr   error
	// completeGate, when non-nil, holds Complete until it is closed, so a
	// test can cancel the workflow while it is in flight.
	completeGate chan struct{}
	// failedReasons are the reasons passed to MarkFailed.
	failedReasons []string
	// deleteObjectsCalls are the Documents the cleanup activity was asked to
	// delete the objects of.
	deleteObjectsCalls []string
	// deleteObjectsFailures is how many times the cleanup activity fails
	// before succeeding.
	deleteObjectsFailures int
	// events records activity starts in order, so a test can see that complete
	// ran after the batches.
	events []string
}

func (s *stubActivities) parseAndChunk(context.Context, string) (int, error) {
	s.mu.Lock()
	s.parseCalls++
	s.events = append(s.events, "parse")
	gate, chunks, err := s.parseGate, s.chunks, s.parseErr
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return chunks, err
}

func (s *stubActivities) embedBatch(ctx context.Context, _ string, from, to int) (int, error) {
	s.mu.Lock()
	s.batchCalls = append(s.batchCalls, batchCall{From: from, To: to, Queue: activity.GetInfo(ctx).TaskQueue})
	s.events = append(s.events, "batch")
	if s.batchErr != nil {
		err := s.batchErr
		s.mu.Unlock()
		return 0, err
	}
	key := [2]int{from, to}
	if s.batchFailures[key] > 0 {
		s.batchFailures[key]--
		err := s.batchFailureErr
		if err == nil {
			err = errors.New("embedding provider timed out")
		}
		s.mu.Unlock()
		return 0, err
	}
	gate := s.batchesGate
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return to - from, nil
}

func (s *stubActivities) complete(context.Context, string) error {
	s.mu.Lock()
	s.completeCalls++
	s.events = append(s.events, "complete")
	gate, err := s.completeGate, s.completeErr
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return err
}

func (s *stubActivities) markFailed(_ context.Context, _, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failedReasons = append(s.failedReasons, reason)
	s.events = append(s.events, "mark_failed")
	return nil
}

func (s *stubActivities) deleteObjects(_ context.Context, documentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteObjectsCalls = append(s.deleteObjectsCalls, documentID)
	s.events = append(s.events, "delete_objects")
	if s.deleteObjectsFailures > 0 {
		s.deleteObjectsFailures--
		return errors.New("object storage unreachable")
	}
	return nil
}

// record adds event to the shared timeline, so a mock callback can show
// where it ran relative to the activities.
func (s *stubActivities) record(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

// deletedIDs returns the Documents the cleanup activity was called for.
func (s *stubActivities) deletedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.deleteObjectsCalls)
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
	env.RegisterActivityWithOptions(stubs.deleteObjects, activity.RegisterOptions{Name: DeleteObjectsActivity})
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

// testNamespace is the namespace the Temporal test environment reports.
const testNamespace = "default-test-namespace"

// runDeleteWorkflow executes the delete workflow with a cancel request that
// answers cancelErr.
func runDeleteWorkflow(t *testing.T, stubs *stubActivities, documentID string, cancelErr error) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := newWorkflowEnv(t, stubs)
	env.OnRequestCancelExternalWorkflow(testNamespace, documentID, "").Return(cancelErr).Once()
	env.ExecuteWorkflow((&DeleteDocumentWorkflow{}).Run, documentID)
	return env
}

func TestDeleteWorkflowCancelsTheIngestThenDeletesTheObjects(t *testing.T) {
	stubs := &stubActivities{}
	env := newWorkflowEnv(t, stubs)
	const documentID = "b1c2d3e4-0000-0000-0000-000000000005"
	env.OnRequestCancelExternalWorkflow(testNamespace, documentID, "").
		Return(func(string, string, string) error {
			stubs.record("cancel")
			return nil
		}).Once()

	env.ExecuteWorkflow((&DeleteDocumentWorkflow{}).Run, documentID)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v, want success", err)
	}
	env.AssertExpectations(t)
	if got := stubs.deletedIDs(); !slices.Equal(got, []string{documentID}) {
		t.Errorf("delete-objects activity ran for %v, want once for %s", got, documentID)
	}
	_, _, _, _, events := stubs.snapshot()
	if want := []string{"cancel", "delete_objects"}; !slices.Equal(events, want) {
		t.Errorf("events = %v, want the cancel request before the delete activity %v", events, want)
	}
}

func TestDeleteWorkflowDeletesObjectsWhenTheIngestWorkflowIsGone(t *testing.T) {
	stubs := &stubActivities{}
	const documentID = "b1c2d3e4-0000-0000-0000-000000000006"
	env := runDeleteWorkflow(t, stubs, documentID, &temporal.UnknownExternalWorkflowExecutionError{})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v, want a missing ingest workflow to be fine", err)
	}
	if got := stubs.deletedIDs(); !slices.Equal(got, []string{documentID}) {
		t.Errorf("delete-objects activity ran for %v, want once for %s", got, documentID)
	}
}

func TestDeleteWorkflowDeletesObjectsWhenTheCancelFails(t *testing.T) {
	stubs := &stubActivities{}
	const documentID = "b1c2d3e4-0000-0000-0000-000000000007"
	env := runDeleteWorkflow(t, stubs, documentID, errors.New("cancel request denied"))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v, want the cleanup to carry on after a failed cancel", err)
	}
	if got := stubs.deletedIDs(); !slices.Equal(got, []string{documentID}) {
		t.Errorf("delete-objects activity ran for %v, want once for %s", got, documentID)
	}
}

func TestDeleteWorkflowRetriesTheDeleteObjectsActivity(t *testing.T) {
	stubs := &stubActivities{deleteObjectsFailures: 1}
	const documentID = "b1c2d3e4-0000-0000-0000-000000000008"
	env := runDeleteWorkflow(t, stubs, documentID, nil)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v, want the retried activity to succeed", err)
	}
	if got := stubs.deletedIDs(); len(got) != 2 {
		t.Errorf("delete-objects activity ran %d times, want an attempt and its retry", len(got))
	}
}

func TestIngestWorkflowCancelledDuringAnEmbedBatchEndsWithoutMarkingFailed(t *testing.T) {
	stubs := &stubActivities{chunks: 7, batchesGate: make(chan struct{})}
	env := newWorkflowEnv(t, stubs)
	env.RegisterDelayedCallback(env.CancelWorkflow, 50*time.Millisecond)
	defer close(stubs.batchesGate)

	env.ExecuteWorkflow((&IngestWorkflow{ModelTaskQueue: testModelTaskQueue, EmbedBatchSize: 3}).Run,
		"b1c2d3e4-0000-0000-0000-000000000009")
	err := env.GetWorkflowError()
	if err == nil || !temporal.IsCanceledError(err) {
		t.Fatalf("workflow error = %v, want the cancelled workflow", err)
	}
	_, _, batches, failed, _ := stubs.snapshot()
	if len(batches) != 3 {
		t.Errorf("embed batches = %d, want the cancellation to arrive with all three in flight", len(batches))
	}
	if len(failed) != 0 {
		t.Errorf("MarkFailed called with %q, want nothing marked for a cancelled ingest", failed)
	}
}

func TestIngestWorkflowCancelledDuringParseEndsWithoutMarkingFailed(t *testing.T) {
	stubs := &stubActivities{chunks: 7, parseGate: make(chan struct{})}
	env := newWorkflowEnv(t, stubs)
	env.RegisterDelayedCallback(env.CancelWorkflow, 50*time.Millisecond)
	defer close(stubs.parseGate)

	env.ExecuteWorkflow((&IngestWorkflow{ModelTaskQueue: testModelTaskQueue, EmbedBatchSize: 3}).Run,
		"b1c2d3e4-0000-0000-0000-00000000000a")
	err := env.GetWorkflowError()
	if err == nil || !temporal.IsCanceledError(err) {
		t.Fatalf("workflow error = %v, want the cancelled workflow", err)
	}
	parse, _, _, failed, _ := stubs.snapshot()
	if parse != 1 {
		t.Errorf("ParseAndChunk ran %d times, want the cancellation to arrive with parse in flight", parse)
	}
	if len(failed) != 0 {
		t.Errorf("MarkFailed called with %q, want nothing marked for a cancelled parse", failed)
	}
}

func TestIngestWorkflowCancelledDuringCompleteEndsWithoutMarkingFailed(t *testing.T) {
	stubs := &stubActivities{chunks: 7, completeGate: make(chan struct{})}
	env := newWorkflowEnv(t, stubs)
	env.RegisterDelayedCallback(env.CancelWorkflow, 50*time.Millisecond)
	defer close(stubs.completeGate)

	env.ExecuteWorkflow((&IngestWorkflow{ModelTaskQueue: testModelTaskQueue, EmbedBatchSize: 3}).Run,
		"b1c2d3e4-0000-0000-0000-00000000000b")
	err := env.GetWorkflowError()
	if err == nil || !temporal.IsCanceledError(err) {
		t.Fatalf("workflow error = %v, want the cancelled workflow", err)
	}
	parse, complete, _, failed, _ := stubs.snapshot()
	if parse != 1 || complete != 1 {
		t.Errorf("parse and complete calls = %d and %d, want the cancellation to arrive with complete in flight", parse, complete)
	}
	if len(failed) != 0 {
		t.Errorf("MarkFailed called with %q, want nothing marked for a cancelled complete", failed)
	}
}

func TestIngestWorkflowCancelledAfterABatchFailureEndsWithoutMarkingFailed(t *testing.T) {
	stubs := &stubActivities{
		chunks:        7,
		batchFailures: map[[2]int]int{{0, 3}: 1},
		batchFailureErr: temporal.NewNonRetryableApplicationError(
			"the Chunks could not be embedded", "test_failure", nil),
		batchesGate: make(chan struct{}),
	}
	env := newWorkflowEnv(t, stubs)
	env.RegisterDelayedCallback(env.CancelWorkflow, 50*time.Millisecond)
	defer close(stubs.batchesGate)

	env.ExecuteWorkflow((&IngestWorkflow{ModelTaskQueue: testModelTaskQueue, EmbedBatchSize: 3}).Run,
		"b1c2d3e4-0000-0000-0000-00000000000c")
	err := env.GetWorkflowError()
	if err == nil || !temporal.IsCanceledError(err) {
		t.Fatalf("workflow error = %v, want a cancelled run even though one batch failed for real", err)
	}
	_, _, batches, failed, _ := stubs.snapshot()
	if len(batches) != 3 {
		t.Errorf("embed batches = %d, want the failed one and the two cancelled while in flight", len(batches))
	}
	if len(failed) != 0 {
		t.Errorf("MarkFailed called with %q, want nothing marked when the workflow is cancelled", failed)
	}
}
