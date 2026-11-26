package ingest

import (
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// NewWorker builds the worker that serves the ingest workflow and its
// activities on taskQueue. Everything is registered under the explicit names
// in ingest.go, never the Go identifiers, so a rename cannot orphan tasks
// that are already queued. The caller starts and stops the worker.
func NewWorker(c client.Client, taskQueue string, activities *Activities) worker.Worker {
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflowWithOptions(IngestWorkflow, workflow.RegisterOptions{Name: WorkflowName})
	w.RegisterActivityWithOptions(activities.ParseAndChunk, activity.RegisterOptions{Name: ParseAndChunkActivity})
	w.RegisterActivityWithOptions(activities.MarkFailed, activity.RegisterOptions{Name: MarkFailedActivity})
	return w
}
