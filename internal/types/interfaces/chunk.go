package interfaces

import (
	"context"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types"
)

// ChunkRepository persists the Chunks of a Document.
type ChunkRepository interface {
	// ReplaceChunks deletes every Chunk of documentID and inserts chunks in
	// the given order. Running it again with the same input leaves the same
	// rows, so a retried ingest activity never duplicates them. It starts no
	// transaction: the caller runs it in one with the status update.
	ReplaceChunks(ctx context.Context, documentID uuid.UUID, chunks []*types.Chunk) error
}
