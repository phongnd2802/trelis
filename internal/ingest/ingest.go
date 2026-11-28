// Package ingest runs the Trelis ingest workflow on Temporal (ADR-0005).
//
// An upload starts one workflow whose ID is the Document ID, which is what
// makes starting the same Document twice safe: a second start joins a run
// that is still going and does not start a completed one again. The workflow
// passes only the Document ID and seq ranges between steps — never Document
// or Chunk content, which would run into payload size limits and history
// growth — and every activity does the I/O. The parse-and-chunk activity
// reads the Source File through the FileService by the Document's object
// key, parses it with the in-process anydoc engine, splits the Markdown into
// Chunks and commits them; the Document stays processing. The workflow then
// starts one embed-batch activity per seq range on the model task queue, each
// of which sends one embeddings request for the Chunks in its range that have
// no vector yet, so the queue's per-second limit is close to a limit on
// embeddings requests. Finally the complete activity checks that no Chunk is
// left without an embedding and only then marks the Document completed.
// A Source File that cannot be parsed, or a Document that cannot be embedded,
// is not retried: the workflow records the reason on the Document and ends.
// A transient model or database failure is retried by the affected activity
// alone, without parsing the Source File again or redoing other batches.
// A cancelled ingest ends without marking anything, because its row is gone.
//
// A second workflow, document.delete, runs once a Document row is deleted:
// it asks Temporal to cancel the Document's ingest (a missing or closed run
// is fine) and then removes every object under the Document's prefix
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
	// DeleteObjectsActivity removes every object under a deleted Document's
	// prefix.
	DeleteObjectsActivity = "document.delete_objects"
)

// ingestWorkflowID is the ID the ingest workflow is started and cancelled
// under: the Document ID (ADR-0005). The delete workflow cancels the ingest by
// the same value, so the two cannot drift apart.
func ingestWorkflowID(documentID string) string { return documentID }

// deleteWorkflowID is the ID the delete cleanup workflow is started under.
func deleteWorkflowID(documentID string) string { return "document-delete/" + documentID }
