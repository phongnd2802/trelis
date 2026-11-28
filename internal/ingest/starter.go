package ingest

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

// Starter starts the Document workflows on Temporal: the ingest workflow and
// the cleanup workflow that runs after a Document is deleted.
type Starter struct {
	client    client.Client
	taskQueue string
}

// NewStarter returns the starter for the workflows that run on taskQueue. The
// ingest workflow's ID is the Document ID (ADR-0005), and the delete
// workflow's is document-delete/{document_id}; a start that is repeated while
// the first run is still going joins that run instead of starting a second
// one or returning an error.
func NewStarter(c client.Client, taskQueue string) *Starter {
	return &Starter{client: c, taskQueue: taskQueue}
}

// StartIngest starts the ingest workflow for documentID. It returns once
// Temporal has accepted the start; the workflow runs in the background.
//
// A running run is reused (USE_EXISTING). A closed one is re-run only if it
// failed or was terminated (ALLOW_DUPLICATE_FAILED_ONLY), so a completed
// Document is never ingested twice while one whose run closed without a
// final status can still be restarted under its own ID. The SDK's default
// WorkflowExecutionErrorWhenAlreadyStarted=false makes a refused start
// return the last run instead of an error.
func (s *Starter) StartIngest(ctx context.Context, documentID uuid.UUID) error {
	return s.start(ctx, ingestWorkflowID(documentID.String()), WorkflowName, documentID.String())
}

// StartDocumentDelete starts the workflow that cancels a deleted Document's
// ingest and removes its objects (ADR-0006). It returns once Temporal has
// accepted the start; the workflow runs in the background. A repeated start
// while the cleanup is still running joins that run, and a cleanup whose run
// closed without finishing can be started again.
func (s *Starter) StartDocumentDelete(ctx context.Context, documentID uuid.UUID) error {
	return s.start(ctx, deleteWorkflowID(documentID.String()), DeleteWorkflowName, documentID.String())
}

// start executes one Document workflow under id. Every Document workflow uses
// the same conflict and reuse policies: an open run is joined, and only a run
// that closed without finishing can be started again.
func (s *Starter) start(ctx context.Context, id, workflowName, documentID string) error {
	_, err := s.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       id,
		TaskQueue:                s.taskQueue,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}, workflowName, documentID)
	if err != nil {
		return fmt.Errorf("start %s workflow for document %s: %w", workflowName, documentID, err)
	}
	return nil
}
