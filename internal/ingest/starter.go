package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// Starter starts the Document workflows on Temporal: the ingest workflow and
// the cleanup workflow that runs after a Document is deleted.
type Starter struct {
	client    client.Client
	taskQueue string
}

// NewStarter returns the starter for the workflows that run on taskQueue. The
// ingest workflow's ID is ingest/{document_id}/{upload_id} (ADR-0005), and the
// delete workflow's is document-delete/{document_id}; a start that is repeated
// while the first run is still going joins that run instead of starting a
// second one or returning an error.
func NewStarter(c client.Client, taskQueue string) *Starter {
	return &Starter{client: c, taskQueue: taskQueue}
}

// StartIngest starts the ingest workflow for one upload of documentID. It
// returns once Temporal has accepted the start; the workflow runs in the
// background.
//
// A running run is reused (USE_EXISTING). A closed one is re-run only if it
// failed or was terminated (ALLOW_DUPLICATE_FAILED_ONLY), so a completed
// upload is never ingested twice while one whose run closed without a final
// status can still be restarted under its own ID. The SDK's default
// WorkflowExecutionErrorWhenAlreadyStarted=false makes a refused start
// return the last run instead of an error.
func (s *Starter) StartIngest(ctx context.Context, documentID, uploadID uuid.UUID) error {
	return s.start(ctx, ingestWorkflowID(documentID.String(), uploadID.String()), WorkflowName,
		documentID.String(), uploadID.String())
}

// TerminateIngest terminates the ingest workflow for one upload of documentID
// if it is still open. A run that never started, or one that already closed,
// is not an error: a re-upload terminates the previous upload's run, which
// may have completed or failed before the replacement landed.
func (s *Starter) TerminateIngest(ctx context.Context, documentID, uploadID uuid.UUID) error {
	err := s.client.TerminateWorkflow(ctx,
		ingestWorkflowID(documentID.String(), uploadID.String()), "", "upload replaced")
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("terminate ingest workflow for document %s upload %s: %w", documentID, uploadID, err)
	}
	return nil
}

// StartDocumentDelete starts the workflow that cancels a deleted Document's
// current ingest and removes its objects (ADR-0006). It returns once Temporal
// has accepted the start; the workflow runs in the background. A repeated
// start while the cleanup is still running joins that run, and a cleanup
// whose run closed without finishing can be started again.
func (s *Starter) StartDocumentDelete(ctx context.Context, documentID, uploadID uuid.UUID) error {
	return s.start(ctx, deleteWorkflowID(documentID.String()), DeleteWorkflowName,
		documentID.String(), uploadID.String())
}

// start executes one Document workflow under id. Every Document workflow uses
// the same conflict and reuse policies: an open run is joined, and only a run
// that closed without finishing can be started again.
func (s *Starter) start(ctx context.Context, id, workflowName string, args ...any) error {
	_, err := s.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       id,
		TaskQueue:                s.taskQueue,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}, workflowName, args...)
	if err != nil {
		return fmt.Errorf("start %s workflow %s: %w", workflowName, id, err)
	}
	return nil
}
