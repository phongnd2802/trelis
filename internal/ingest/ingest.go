// Package ingest runs the Trelis ingest workflow on Temporal (ADR-0005).
//
// An upload starts one workflow whose ID carries the Document ID and the
// upload ID, which is what makes starting the same upload twice safe: a
// second start joins a run that is still going and does not start a completed
// one again, while a re-upload starts a new run and terminates the previous
// one. The workflow passes only the IDs and seq ranges between steps — never
// Document or Chunk content, which would run into payload size limits and
// history growth — and every activity does the I/O. The parse-and-chunk
// activity reads the Source File through the FileService by the Document's
// object key, parses it with the in-process anydoc engine, splits the
// Markdown into Chunks and commits them; the Document stays processing. It
// then removes the objects of earlier uploads, because the new upload's
// Chunks are committed and the previous bytes are no longer needed. The
// workflow starts one embed-batch activity per seq range on the model task
// queue, each of which sends one embeddings request for the Chunks in its
// range that have no vector yet, so the queue's per-second limit is close to
// a limit on embeddings requests. Finally the complete activity checks that
// no Chunk is left without an embedding and only then marks the Document
// completed. Every step also records an Ingest Event in Postgres
// (internal/types): the parse activity a parse_started for each attempt and a
// parse_finished in the transaction that commits the Chunks, each embed batch
// an embed_batch_finished in the transaction that stores its vectors, the
// complete and mark-failed activities completed or failed in the transaction
// that sets the status, and a re-upload an upload_replaced. Each event names
// the upload it belongs to; an activity checks the Document still points at
// its upload in the same transaction that writes, so a run that a re-upload
// superseded writes nothing. A client follows them over the SSE stream of
// GET /documents/{id}/events and can resume from its Last-Event-ID (#23).
// A Source File that cannot be parsed, or a Document that cannot be embedded,
// is not retried: the workflow records the reason on the Document and ends.
// A transient model or database failure is retried by the affected activity
// alone, without parsing the Source File again or redoing other batches.
// A cancelled ingest, or one whose upload was superseded, ends without
// marking anything.
//
// A second workflow, document.delete, runs once a Document row is deleted:
// it asks Temporal to cancel the current upload's ingest (a missing or closed
// run is fine) and then removes every object under the Document's prefix
// (ADR-0006).
package ingest

// Names the workflows and their activities are registered and started under.
// They are explicit and stable, so renaming a Go method cannot orphan tasks
// already queued in Temporal.
const (
	// WorkflowName is the ingest workflow's registered name.
	WorkflowName = "ingest"
	// DeleteWorkflowName is the registered name of the workflow that cancels
	// a deleted Document's ingest and removes its objects.
	DeleteWorkflowName = "document.delete"
	// ParseAndChunkActivity parses a Source File and stores its Chunks.
	ParseAndChunkActivity = "ingest.parse_and_chunk"
	// EmbedBatchActivity embeds one seq range of a Document's Chunks.
	EmbedBatchActivity = "ingest.embed_batch"
	// CompleteActivity marks a Document completed once no Chunk lacks an
	// embedding.
	CompleteActivity = "ingest.complete"
	// MarkFailedActivity records why a Document's ingest failed.
	MarkFailedActivity = "ingest.mark_failed"
	// DeleteSupersededUploadsActivity removes the objects of a Document's
	// earlier uploads once the current upload's Chunks are committed.
	DeleteSupersededUploadsActivity = "ingest.delete_superseded_uploads"
	// DeleteObjectsActivity removes every object under a deleted Document's
	// prefix.
	DeleteObjectsActivity = "document.delete_objects"
)

// ingestWorkflowID is the ID the ingest workflow is started and terminated
// under: ingest/{document_id}/{upload_id} (ADR-0005). Each upload is its own
// run, so a re-upload never joins the run it replaces. The delete workflow
// terminates the current upload's run by the same value, so the two cannot
// drift apart.
func ingestWorkflowID(documentID, uploadID string) string {
	return "ingest/" + documentID + "/" + uploadID
}

// deleteWorkflowID is the ID the delete cleanup workflow is started under.
func deleteWorkflowID(documentID string) string { return "document-delete/" + documentID }
