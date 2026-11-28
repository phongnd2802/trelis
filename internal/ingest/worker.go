package ingest

import (
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// NewWorker builds the worker that serves the ingest and Document delete
// workflows and their non-model activities on taskQueue. The ingest workflow
// schedules the embed batches on modelTaskQueue, which NewModelWorker serves,
// in ranges of embedBatchSize Chunks (zero means
// config.DefaultEmbeddingBatchSize). Everything is registered under the
// explicit names in ingest.go, never the Go identifiers, so a rename cannot
// orphan tasks that are already queued. The caller starts and stops the
// worker.
func NewWorker(c client.Client, taskQueue, modelTaskQueue string, embedBatchSize int, activities *Activities) worker.Worker {
	w := worker.New(c, taskQueue, worker.Options{})
	ingestWorkflow := &IngestWorkflow{ModelTaskQueue: modelTaskQueue, EmbedBatchSize: embedBatchSize}
	w.RegisterWorkflowWithOptions(ingestWorkflow.Run, workflow.RegisterOptions{Name: WorkflowName})
	w.RegisterWorkflowWithOptions((&DeleteDocumentWorkflow{}).Run, workflow.RegisterOptions{Name: DeleteWorkflowName})
	w.RegisterActivityWithOptions(activities.ParseAndChunk, activity.RegisterOptions{Name: ParseAndChunkActivity})
	w.RegisterActivityWithOptions(activities.Complete, activity.RegisterOptions{Name: CompleteActivity})
	w.RegisterActivityWithOptions(activities.MarkFailed, activity.RegisterOptions{Name: MarkFailedActivity})
	w.RegisterActivityWithOptions(activities.DeleteSupersededUploads, activity.RegisterOptions{Name: DeleteSupersededUploadsActivity})
	w.RegisterActivityWithOptions(activities.DeleteObjects, activity.RegisterOptions{Name: DeleteObjectsActivity})
	return w
}

// NewModelWorker builds the worker for model-calling activities on taskQueue,
// limiting activity starts to activitiesPerSecond across every worker on that
// queue. Temporal enforces the limit, so it holds for a whole deployment and
// not just this process. Only activities that call a model are registered
// here. The caller starts and stops the worker.
func NewModelWorker(c client.Client, taskQueue string, activitiesPerSecond float64, activities *Activities) worker.Worker {
	w := worker.New(c, taskQueue, worker.Options{TaskQueueActivitiesPerSecond: activitiesPerSecond})
	w.RegisterActivityWithOptions(activities.EmbedBatch, activity.RegisterOptions{Name: EmbedBatchActivity})
	return w
}
