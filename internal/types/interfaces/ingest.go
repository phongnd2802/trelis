package interfaces

import (
	"context"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types"
)

// IngestStarter starts and terminates the ingest workflow for one upload of a
// Document (ADR-0005). The workflow ID carries the Document and upload IDs,
// so starting an upload whose ingest is already running or completed does not
// start a second run.
type IngestStarter interface {
	// StartIngest starts the ingest workflow for one upload of documentID. It
	// returns once Temporal has accepted the start and does not wait for the
	// workflow. A running ingest is joined, a completed one is not started
	// again, and one whose run failed can be retried.
	StartIngest(ctx context.Context, documentID, uploadID uuid.UUID) error
	// TerminateIngest terminates the ingest workflow for one upload, if it is
	// still open. A run that does not exist, or one that already closed, is
	// not an error: a re-upload terminates the previous upload's run, which
	// may have finished or never started.
	TerminateIngest(ctx context.Context, documentID, uploadID uuid.UUID) error
}

// DocumentDeleteStarter starts the workflow that cleans up after a deleted
// Document (ADR-0006): it cancels the current upload's ingest and removes
// every object under documents/{document_id}/. Its workflow ID is
// document-delete/{document_id}, so a repeated start joins an open run.
type DocumentDeleteStarter interface {
	// StartDocumentDelete starts the cleanup workflow for documentID and the
	// upload whose ingest it must cancel. It returns once Temporal has
	// accepted the start and does not wait for the workflow.
	StartDocumentDelete(ctx context.Context, documentID, uploadID uuid.UUID) error
}

// IngestService is the use cases the ingest activities call. Activities are a
// boundary like HTTP handlers: they call the service, and only the service
// starts transactions (ADR-0007).
type IngestService interface {
	// ParseAndChunk parses one upload of a Document's Source File, splits the
	// Markdown into Chunks and, in one transaction, replaces the Document's
	// Chunks and Parent Chunks and leaves it processing. It reports what it
	// stored and how the tier chain chose it. It records a parse_started
	// Ingest Event for attempt and a parse_finished one in the transaction
	// that commits the Chunks; each is committed only while uploadID is still
	// the Document's current upload. A *types.UnparseableError means the
	// Source File cannot be parsed, a *types.DocumentNotFoundError means the
	// Document is gone and a *types.SupersededUploadError means a newer upload
	// owns it; none can be fixed by retrying. Any other error is retryable.
	ParseAndChunk(ctx context.Context, documentID, uploadID uuid.UUID, attempt int) (types.ChunkingResult, error)
	// EmbedBatch embeds the Chunks of one upload of a Document whose Seq is in
	// [from, to) and that have no embedding yet, in one embedding call, and
	// stores their vectors in one transaction with an embed_batch_finished
	// event, but only while uploadID is still the current upload. It returns
	// how many Chunks it embedded; a range that is already embedded is a
	// no-op. A *types.EmbeddingUnconfiguredError means no embedding model is
	// set, and a *types.SupersededUploadError means a newer upload owns the
	// Document; neither can be fixed by retrying. Any other error is
	// retryable.
	EmbedBatch(ctx context.Context, documentID, uploadID uuid.UUID, from, to int) (int, error)
	// Complete marks one upload of a Document completed once every Chunk has
	// an embedding, but only while uploadID is still the current upload. One
	// or more unembedded Chunks is a retryable error naming the count, so a
	// Document is never completed with a missing vector. A
	// *types.DocumentNotFoundError means the row is gone and a
	// *types.SupersededUploadError means a newer upload owns it; neither can
	// be fixed by retrying.
	Complete(ctx context.Context, documentID, uploadID uuid.UUID) error
	// MarkFailed records that one upload of a Document failed with reason,
	// but only while uploadID is still the current upload: a superseded run
	// must never mark the new upload failed. A Document deleted since the
	// failure is not an error.
	MarkFailed(ctx context.Context, documentID, uploadID uuid.UUID, reason string) error
	// DeleteSupersededUploads removes every object of a Document's earlier
	// uploads, once the current upload's Chunks are committed. It first
	// checks, under the Document row lock, that uploadID is still current; a
	// *types.SupersededUploadError means a newer upload owns the Document and
	// nothing was deleted. It is idempotent.
	DeleteSupersededUploads(ctx context.Context, documentID, uploadID uuid.UUID) error
}

// DocumentParser turns a Source File's bytes into Markdown. It exists so the
// ingest service can be tested without the anydoc engine.
type DocumentParser interface {
	// Parse converts req's file. A returned error means the file could not be
	// parsed.
	Parse(ctx context.Context, req *types.ParseRequest) (*types.ParseResult, error)
}
