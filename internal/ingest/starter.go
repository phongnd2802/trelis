package ingest

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// starter starts ingest workflows with the Document ID as the workflow ID.
type starter struct {
	client    client.Client
	taskQueue string
}

// NewStarter returns the IngestStarter that starts workflows on taskQueue.
// The workflow ID is the Document ID (ADR-0005), so a start that is repeated
// while the first run is still going joins that run instead of starting a
// second one or returning an error.
func NewStarter(c client.Client, taskQueue string) interfaces.IngestStarter {
	return &starter{client: c, taskQueue: taskQueue}
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
func (s *starter) StartIngest(ctx context.Context, documentID uuid.UUID) error {
	_, err := s.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       documentID.String(),
		TaskQueue:                s.taskQueue,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}, WorkflowName, documentID.String())
	if err != nil {
		return fmt.Errorf("start ingest workflow for document %s: %w", documentID, err)
	}
	return nil
}
