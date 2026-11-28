package ingest

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/phongnd2802/trelis/internal/config"
)

const (
	// parseAndChunkTimeout bounds one parse-and-chunk attempt; a large Source
	// File plus its parse can take minutes.
	parseAndChunkTimeout = 10 * time.Minute
	// embedBatchTimeout bounds one embed-batch attempt: one embeddings request
	// for up to EmbedBatchSize Chunks, plus the client's own retries on 429 or
	// 5xx inside that attempt. It needs far less than a parse does.
	embedBatchTimeout = 5 * time.Minute
	// activityAttempts is how often a retryable activity failure is retried
	// before the Document is marked failed.
	activityAttempts = 5
	// markFailedTimeout bounds the bookkeeping activity; it is short because
	// the workflow has no work left even if it fails.
	markFailedTimeout = time.Minute
	// markFailedAttempts is how often marking a Document failed is retried.
	markFailedAttempts = 3
	// deleteObjectsTimeout bounds one delete-objects attempt; a large prefix
	// may take several delete requests.
	deleteObjectsTimeout = 10 * time.Minute
	// deleteObjectsAttempts is how often the delete-objects activity is
	// retried before the cleanup workflow fails.
	deleteObjectsAttempts = 5
	// maxReasonLength keeps a stored failure reason short enough to show
	// beside the Document.
	maxReasonLength = 500
)

// IngestWorkflow is the ingest workflow definition. It holds the model task
// queue and batch size because ActivityOptions and the fan-out shape are
// workflow code, not activity input: every run of one deployed version must
// schedule the same batches on the same queue for the rate limit to hold.
type IngestWorkflow struct {
	// ModelTaskQueue is the queue the embed batches run on, where
	// temporal.model_activities_per_second applies.
	ModelTaskQueue string
	// EmbedBatchSize is how many Chunks one embed-batch activity sends to the
	// provider. Zero falls back to config.DefaultEmbeddingBatchSize.
	EmbedBatchSize int
}

// Run parses and chunks one Document, embeds every Chunk in batches on
// ModelTaskQueue, then marks the Document completed once no embedding is
// missing. Parse-and-chunk runs with retries; when it still fails, the
// workflow marks the Document failed with a short reason and ends. Each batch
// is scheduled up front and retried alone, so one transient model failure
// neither re-parses the Document nor repeats the batches that succeeded. A
// failed batch, or a complete step that still sees an unembedded Chunk, marks
// the Document failed exactly as a failed parse does. A Document that no
// longer exists is the exception: there is no row to mark, so the workflow
// just ends. The input is only the Document ID and the batches pass only seq
// ranges (ADR-0005). The workflow does no I/O and reads no clock, so it stays
// deterministic.
func (w *IngestWorkflow) Run(ctx workflow.Context, documentID string) error {
	log := workflow.GetLogger(ctx)
	// No workflow.GetVersion is needed for the Ingest Events (#23): they are
	// written inside the existing activities, so the command sequence this
	// workflow schedules is unchanged and no run in flight replays against
	// different history.
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: parseAndChunkTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: activityAttempts},
	})

	var chunks int
	if err := workflow.ExecuteActivity(ctx, ParseAndChunkActivity, documentID).Get(ctx, &chunks); err != nil {
		return w.fail(ctx, documentID, err)
	}

	// No workflow.GetVersion is needed: #22 has not shipped, so no run of the
	// earlier one-embed-activity workflow is replayed against this code.
	batchSize := w.EmbedBatchSize
	if batchSize < 1 {
		batchSize = config.DefaultEmbeddingBatchSize
	}
	batchCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: embedBatchTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: activityAttempts},
	})
	batchCtx = workflow.WithTaskQueue(batchCtx, w.ModelTaskQueue)

	batches := make([]workflow.Future, 0, (chunks+batchSize-1)/batchSize)
	for from := 0; from < chunks; from += batchSize {
		to := min(from+batchSize, chunks)
		batches = append(batches, workflow.ExecuteActivity(batchCtx, EmbedBatchActivity, documentID, from, to))
	}
	// Wait for every batch even after one fails: each future has already been
	// scheduled, and the first error is the one the Document records.
	var batchErr error
	for _, batch := range batches {
		if err := batch.Get(batchCtx, nil); err != nil && batchErr == nil {
			batchErr = err
		}
	}
	if batchErr != nil {
		return w.fail(ctx, documentID, batchErr)
	}

	if err := workflow.ExecuteActivity(ctx, CompleteActivity, documentID).Get(ctx, nil); err != nil {
		return w.fail(ctx, documentID, err)
	}
	log.Info("ingest finished", "document", documentID)
	return nil
}

// DeleteDocumentWorkflow is the cleanup workflow started after a Document
// row is deleted. It first asks Temporal to cancel the Document's ingest
// workflow, whose ID is the Document ID. A run that is missing or already
// closed is fine; any other cancel failure is logged and the workflow carries
// on, because removing the objects does not depend on the cancel. It then
// runs the idempotent delete-objects activity. The workflow passes only the
// Document ID and does no I/O, so it stays deterministic.
type DeleteDocumentWorkflow struct{}

// Run cancels the ingest workflow for documentID, then removes every object
// under the Document's prefix. A failed delete-objects activity fails the
// workflow after Temporal has retried it.
func (w *DeleteDocumentWorkflow) Run(ctx workflow.Context, documentID string) error {
	log := workflow.GetLogger(ctx)
	if err := workflow.RequestCancelExternalWorkflow(ctx, ingestWorkflowID(documentID), "").Get(ctx, nil); err != nil {
		var unknown *temporal.UnknownExternalWorkflowExecutionError
		if !errors.As(err, &unknown) {
			log.Warn("document delete: could not cancel the ingest workflow",
				"document", documentID, "error", err)
		}
	}
	deleteCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: deleteObjectsTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: deleteObjectsAttempts},
	})
	if err := workflow.ExecuteActivity(deleteCtx, DeleteObjectsActivity, documentID).Get(deleteCtx, nil); err != nil {
		return fmt.Errorf("delete objects of document %s: %w", documentID, err)
	}
	return nil
}

// fail records cause on the Document and returns it, except for a Document
// that no longer exists: there is no row left to mark. ctx carries the
// workflow's default activity options, so MarkFailed runs on the ingest queue
// rather than the model queue.
func (w *IngestWorkflow) fail(ctx workflow.Context, documentID string, cause error) error {
	log := workflow.GetLogger(ctx)
	// A cancellation means the Document was deleted: its row is gone, and a
	// MarkFailed run with the cancelled context would fail anyway. Return the
	// cancellation without marking anything. The context is checked as well as
	// the cause: a batch that failed for real before the cancel arrived still
	// calls fail with its own error, and that run must close cancelled too. No
	// workflow.GetVersion is needed: nothing before #24 cancelled an ingest, so
	// no run in flight has a cancellation in its history to replay against this
	// branch.
	if err := ctx.Err(); errors.Is(err, workflow.ErrCanceled) {
		log.Info("ingest cancelled; the Document is gone", "document", documentID)
		return err
	}
	if temporal.IsCanceledError(cause) {
		log.Info("ingest cancelled; the Document is gone", "document", documentID)
		return cause
	}
	if applicationErrorType(cause) == documentNotFoundType {
		log.Warn("the Document no longer exists; nothing to mark", "document", documentID)
		return fmt.Errorf("ingest document %s: %w", documentID, cause)
	}
	reason := failureReason(cause)
	log.Warn("ingest failed; marking the Document failed", "document", documentID, "reason", reason)
	failCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: markFailedTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: markFailedAttempts},
	})
	if err := workflow.ExecuteActivity(failCtx, MarkFailedActivity, documentID, reason).Get(failCtx, nil); err != nil {
		return fmt.Errorf("mark document %s failed: %w", documentID, err)
	}
	return fmt.Errorf("ingest document %s: %w", documentID, cause)
}

// failureReason turns an activity failure into the short reason stored on the
// Document. Only an application error type whose message was chosen to be
// shown carries one; for every other failure Temporal puts the raw Go error
// text in Message(), which can name an endpoint, bucket or key that must not
// reach a reader, so it gets the generic sentence.
func failureReason(err error) string {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		switch appErr.Type() {
		case unparseableDocumentType, embeddingUnconfiguredType:
			if msg := strings.TrimSpace(appErr.Message()); msg != "" {
				return truncate(msg)
			}
		}
	}
	return "the Document could not be ingested"
}

// applicationErrorType returns the Temporal application error type in err, or
// "" when err is not an application error.
func applicationErrorType(err error) string {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		return appErr.Type()
	}
	return ""
}

// truncate caps a reason at maxReasonLength runes, marked as shortened.
func truncate(reason string) string {
	runes := []rune(reason)
	if len(runes) <= maxReasonLength {
		return reason
	}
	return string(runes[:maxReasonLength-1]) + "…"
}
