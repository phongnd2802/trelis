package ingest

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	// activityTimeout bounds one parse-and-chunk attempt; a large Source File
	// plus its parse can take minutes.
	activityTimeout = 10 * time.Minute
	// activityAttempts is how often a retryable activity failure is retried
	// before the Document is marked failed.
	activityAttempts = 5
	// markFailedTimeout bounds the bookkeeping activity; it is short because
	// the workflow has no work left even if it fails.
	markFailedTimeout = time.Minute
	// markFailedAttempts is how often marking a Document failed is retried.
	markFailedAttempts = 3
	// maxReasonLength keeps a stored failure reason short enough to show
	// beside the Document.
	maxReasonLength = 500
)

// IngestWorkflow parses and chunks one Document and records whether it
// succeeded, in that order: the parse-and-chunk activity runs with retries;
// when it still fails, the workflow marks the Document failed with a short
// reason and ends. A Document that no longer exists is the exception: there
// is no row to mark, so the workflow just ends. The input is only the
// Document ID (ADR-0005). The workflow does no I/O and reads no clock, so it
// stays deterministic.
func IngestWorkflow(ctx workflow.Context, documentID string) error {
	log := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: activityTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: activityAttempts},
	})
	if err := workflow.ExecuteActivity(ctx, ParseAndChunkActivity, documentID).Get(ctx, nil); err != nil {
		if applicationErrorType(err) == documentNotFoundType {
			log.Warn("the Document no longer exists; nothing to mark", "document", documentID)
			return fmt.Errorf("ingest document %s: %w", documentID, err)
		}
		reason := failureReason(err)
		log.Warn("ingest failed; marking the Document failed", "document", documentID, "reason", reason)
		failCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: markFailedTimeout,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: markFailedAttempts},
		})
		if markErr := workflow.ExecuteActivity(failCtx, MarkFailedActivity, documentID, reason).Get(failCtx, nil); markErr != nil {
			return fmt.Errorf("mark document %s failed: %w", documentID, markErr)
		}
		return fmt.Errorf("ingest document %s: %w", documentID, err)
	}
	return nil
}

// failureReason turns an activity failure into the short reason stored on the
// Document. Only the parse activity's own application error type carries a
// message chosen to be shown; for every other failure Temporal puts the raw
// Go error text in Message(), which can name an endpoint, bucket or key that
// must not reach a reader, so it gets the generic sentence.
func failureReason(err error) string {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.Type() == unparseableDocumentType {
		if msg := strings.TrimSpace(appErr.Message()); msg != "" {
			return truncate(msg)
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
