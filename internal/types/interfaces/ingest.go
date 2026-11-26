package interfaces

import (
	"context"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types"
)

// IngestStarter starts the ingest workflow for a Document (ADR-0005). The
// workflow ID is the Document ID, so starting a Document whose ingest is
// already running or completed does not start a second run.
type IngestStarter interface {
	// StartIngest starts the ingest workflow for documentID. It returns once
	// Temporal has accepted the start and does not wait for the workflow. A
	// running ingest is joined, a completed one is not started again, and one
	// whose run failed can be retried.
	StartIngest(ctx context.Context, documentID uuid.UUID) error
}

// IngestService is the use cases the ingest activities call. Activities are a
// boundary like HTTP handlers: they call the service, and only the service
// starts transactions (ADR-0007).
type IngestService interface {
	// ParseAndChunk parses a Document's Source File, splits the Markdown into
	// Chunks and, in one transaction, replaces the Document's Chunks and
	// marks it completed. It returns how many Chunks it stored. A
	// *types.UnparseableError means the Source File cannot be parsed, and a
	// *types.DocumentNotFoundError means the Document is gone; neither can be
	// fixed by retrying. Any other error is retryable.
	ParseAndChunk(ctx context.Context, documentID uuid.UUID) (int, error)
	// MarkFailed records that a Document's ingest failed with reason.
	MarkFailed(ctx context.Context, documentID uuid.UUID, reason string) error
}

// DocumentParser turns a Source File's bytes into Markdown. It exists so the
// ingest service can be tested without the anydoc engine.
type DocumentParser interface {
	// Parse converts req's file. A returned error means the file could not be
	// parsed.
	Parse(ctx context.Context, req *types.ParseRequest) (*types.ParseResult, error)
}
