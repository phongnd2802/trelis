package interfaces

import (
	"context"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types"
)

// ChunkRepository persists the Chunks of a Document.
type ChunkRepository interface {
	// ReplaceChunks deletes every Parent Chunk and Chunk of documentID and
	// inserts set's Parent Chunks in Seq order, then its Chunks with their
	// Parent Chunk links. It deletes the Chunks before the Parent Chunks;
	// switching parent-child mode off and replacing the set therefore leaves no
	// orphan Parent Chunk. Running it again with the same input leaves the same
	// rows, so a retried ingest activity never duplicates them. It starts no
	// transaction: the caller runs it in one with the status update.
	ReplaceChunks(ctx context.Context, documentID uuid.UUID, set types.ChunkSet) error

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

	// SearchBM25 returns up to topK Chunks from any of documentIDs that match
	// query, most relevant by pg_search's BM25 score first, ties broken by
	// Chunk ID. The query is tokenized like the indexed text, so diacritics
	// are optional and punctuation is only a separator; a query whose tokens
	// match nothing returns an empty slice, not an error. documentIDs limits
	// the search itself, so a Chunk of another Document never takes a top-K
	// place.
	SearchBM25(ctx context.Context, documentIDs []uuid.UUID, query string, topK int) ([]*types.ChunkMatch, error)
}

// ChunkReader reads one Chunk for a Principal, always within the Documents
// they can read (ADR-0004).
type ChunkReader interface {
	// GetReadableChunk returns the Chunk when principalID can read its
	// Document, else ErrChunkNotFound. A Chunk that does not exist and one
	// the Principal cannot read are the same error, so the answer reveals
	// nothing about what exists.
	GetReadableChunk(ctx context.Context, principalID, id uuid.UUID) (*types.Chunk, error)
}

// ChunkService is the Chunk read use case, always on behalf of the Principal
// in ctx (types.WithPrincipal). It answers with *errors.AppError for anything
// the client should see.
type ChunkService interface {
	// GetChunkByID returns a Chunk the caller can read; one that does not
	// exist and one they cannot read are the same not-found error.
	GetChunkByID(ctx context.Context, id uuid.UUID) (*types.Chunk, error)
}
