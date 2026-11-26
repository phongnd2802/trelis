// Package ingest runs the Trelis ingest workflow on Temporal (ADR-0005).
//
// An upload starts one workflow whose ID is the Document ID, which is what
// makes starting the same Document twice safe: a second start joins a run
// that is still going and does not start a completed one again. The workflow
// passes only the Document ID between steps — never Document or Chunk
// content, which would run into payload size limits and history growth — and
// every activity does the I/O. The parse activity reads the Source File
// through the FileService by the Document's object key, parses it with the
// in-process anydoc engine, splits the Markdown into Chunks and commits them
// with the Document's completed status.
// A Source File that cannot be parsed is not retried: the workflow records
// the reason on the Document and ends.
package ingest

// Names the workflow and its activities are registered and started under.
// They are explicit and stable, so renaming a Go method cannot orphan tasks
// already queued in Temporal.
const (
	// WorkflowName is the ingest workflow's registered name.
	WorkflowName = "ingest"
	// ParseAndChunkActivity parses a Source File and stores its Chunks.
	ParseAndChunkActivity = "ingest.parse_and_chunk"
	// MarkFailedActivity records why a Document's ingest failed.
	MarkFailedActivity = "ingest.mark_failed"
)
