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

	// ListUnembeddedChunks returns the Chunks of documentID whose Seq is in
	// [from, to) and that have no embedding yet, in seq order. It fills ID,
	// Seq, Content, ContextHeader, StartOffset and EndOffset. A range that is
	// already embedded, or outside the Document, gives an empty list.
	ListUnembeddedChunks(ctx context.Context, documentID uuid.UUID, from, to int) ([]*types.Chunk, error)

	// SetChunkEmbedding stores the vector model produced for the Chunk
	// chunkID. It starts no transaction: the caller runs every vector of one
	// batch in one. It returns an error naming chunkID when the Chunk no
	// longer exists, so a caller never treats a changed Chunk set as done.
	SetChunkEmbedding(ctx context.Context, chunkID uuid.UUID, vector []float32, model string) error

	// CountChunksWithoutEmbedding returns how many of documentID's Chunks
	// have no embedding yet.
	CountChunksWithoutEmbedding(ctx context.Context, documentID uuid.UUID) (int, error)
}

// ChunkSearcher finds Chunks for retrieval. It is the read side of the Chunk
// repository: the retrieval service depends only on it, never on the ingest
// writes.
type ChunkSearcher interface {
	// SearchDense returns up to topK Chunks from any of documentIDs whose
	// embedding was produced by model, most cosine-similar to queryVector
	// first, ties broken by Chunk ID. Chunks with no embedding, or one by
	// another model, are skipped because their vectors may not even share a
	// dimension with the query's.
	SearchDense(ctx context.Context, documentIDs []uuid.UUID, queryVector []float32, model string, topK int) ([]*types.ChunkMatch, error)
}
