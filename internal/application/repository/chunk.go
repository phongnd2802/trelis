package repository

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/sqlc"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

type chunkRepository struct {
	store db.Store
}

// NewChunkRepository returns a ChunkRepository on store.
func NewChunkRepository(store db.Store) interfaces.ChunkRepository {
	return &chunkRepository{store: store}
}

// NewChunkSearcher returns the read side of the Chunk repository: the dense
// search the retrieval service runs.
func NewChunkSearcher(store db.Store) interfaces.ChunkSearcher {
	return &chunkRepository{store: store}
}

// ReplaceChunks deletes a Document's Chunks and inserts chunks in order. It
// starts no transaction of its own: call it inside the service's ExecTx so a
// failed insert leaves the previous set in place.
func (r *chunkRepository) ReplaceChunks(ctx context.Context, documentID uuid.UUID, chunks []*types.Chunk) error {
	q := r.store.Querier(ctx)
	if err := q.DeleteChunksByDocument(ctx, documentID); err != nil {
		return fmt.Errorf("delete chunks of document %s: %w", documentID, err)
	}
	for _, c := range chunks {
		err := q.InsertChunk(ctx, sqlc.InsertChunkParams{
			DocumentID:    documentID,
			Seq:           int32(c.Seq),
			Content:       c.Content,
			ContextHeader: c.ContextHeader,
			StartOffset:   int32(c.StartOffset),
			EndOffset:     int32(c.EndOffset),
		})
		if err != nil {
			return fmt.Errorf("insert chunk %d of document %s: %w", c.Seq, documentID, err)
		}
	}
	return nil
}

// ListUnembeddedChunks returns a Document's Chunks in [from, to) that still
// need embedding, in seq order. Re-running a range therefore skips the Chunks
// an earlier attempt already stored, and an empty result is a no-op for the
// caller. The vector itself is not selected: no application code reads a
// stored embedding, and keeping it out of the scan keeps pgvector's type out
// of the query.
func (r *chunkRepository) ListUnembeddedChunks(ctx context.Context, documentID uuid.UUID, from, to int) ([]*types.Chunk, error) {
	rows, err := r.store.Querier(ctx).ListUnembeddedChunksByDocument(ctx, sqlc.ListUnembeddedChunksByDocumentParams{
		DocumentID: documentID,
		FromSeq:    int32(from),
		ToSeq:      int32(to),
	})
	if err != nil {
		return nil, fmt.Errorf("list unembedded chunks %d-%d of document %s: %w", from, to, documentID, err)
	}
	chunks := make([]*types.Chunk, 0, len(rows))
	for _, row := range rows {
		chunks = append(chunks, &types.Chunk{
			ID:            row.ID,
			DocumentID:    documentID,
			Seq:           int(row.Seq),
			Content:       row.Content,
			ContextHeader: row.ContextHeader,
			StartOffset:   int(row.StartOffset),
			EndOffset:     int(row.EndOffset),
		})
	}
	return chunks, nil
}

// CountChunksWithoutEmbedding counts a Document's Chunks that have no vector.
// Complete uses it so no Document is marked completed with a missing
// embedding.
func (r *chunkRepository) CountChunksWithoutEmbedding(ctx context.Context, documentID uuid.UUID) (int, error) {
	count, err := r.store.Querier(ctx).CountChunksWithoutEmbedding(ctx, documentID)
	if err != nil {
		return 0, fmt.Errorf("count unembedded chunks of document %s: %w", documentID, err)
	}
	return int(count), nil
}

// SetChunkEmbedding stores vector and the model that produced it on a Chunk.
// It starts no transaction of its own: call it inside the service's ExecTx so
// a failed update leaves no half-embedded batch. A Chunk that no longer exists
// is an error, not a silent skip, so the activity retries against the Chunk set
// it has now.
func (r *chunkRepository) SetChunkEmbedding(ctx context.Context, chunkID uuid.UUID, vector []float32, model string) error {
	rows, err := r.store.Querier(ctx).SetChunkEmbedding(ctx, sqlc.SetChunkEmbeddingParams{
		ID:             chunkID,
		Embedding:      vectorText(vector),
		EmbeddingModel: model,
	})
	if err != nil {
		return fmt.Errorf("set embedding of chunk %s: %w", chunkID, err)
	}
	if rows == 0 {
		return fmt.Errorf("set embedding of chunk %s: the Chunk no longer exists", chunkID)
	}
	return nil
}

// SearchDense returns the Chunks of documentIDs embedded by model, most
// cosine-similar to queryVector first, ties broken by Chunk ID. The vector
// travels as its text form and is cast in the query, like SetChunkEmbedding,
// which keeps pgvector's type out of the Go code. A Chunk with no embedding,
// or one produced by another model, is skipped: its vector may not share a
// dimension with the query's.
func (r *chunkRepository) SearchDense(ctx context.Context, documentIDs []uuid.UUID,
	queryVector []float32, model string, topK int,
) ([]*types.ChunkMatch, error) {
	rows, err := r.store.Querier(ctx).SearchDenseChunks(ctx, sqlc.SearchDenseChunksParams{
		QueryEmbedding: vectorText(queryVector),
		DocumentIds:    documentIDs,
		EmbeddingModel: model,
		TopK:           int32(topK),
	})
	if err != nil {
		return nil, fmt.Errorf("search dense chunks of %d documents: %w", len(documentIDs), err)
	}
	matches := make([]*types.ChunkMatch, 0, len(rows))
	for _, row := range rows {
		matches = append(matches, &types.ChunkMatch{
			Chunk: &types.Chunk{
				ID:            row.ID,
				DocumentID:    row.DocumentID,
				Seq:           int(row.Seq),
				Content:       row.Content,
				ContextHeader: row.ContextHeader,
			},
			Score: row.Score,
		})
	}
	return matches, nil
}

// vectorText renders vector in pgvector's text form, [0.1,0.2], which the
// query casts to vector. Formatting float32 with its own bit size keeps the
// value the model returned; %g uses the shortest form that reads back the same.
func vectorText(vector []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range vector {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
