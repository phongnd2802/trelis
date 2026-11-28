package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/sqlc"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// ErrChunkNotFound means no Chunk matched, or the Principal asking cannot
// read its Document.
var ErrChunkNotFound = errors.New("chunk not found")

type chunkRepository struct {
	store db.Store
}

// NewChunkRepository returns a ChunkRepository on store.
func NewChunkRepository(store db.Store) interfaces.ChunkRepository {
	return &chunkRepository{store: store}
}

// NewChunkSearcher returns the read side of the Chunk repository: the dense
// and BM25 searches the retrieval service runs.
func NewChunkSearcher(store db.Store) interfaces.ChunkSearcher {
	return &chunkRepository{store: store}
}

// NewChunkReader returns the Chunk repository's read-by-ID side.
func NewChunkReader(store db.Store) interfaces.ChunkReader {
	return &chunkRepository{store: store}
}

// GetReadableChunk returns the Chunk when principalID can read its Document.
// The readable check is part of the query, so a Chunk of a Restricted
// Document and an unknown ID are the same ErrChunkNotFound (ADR-0004).
func (r *chunkRepository) GetReadableChunk(ctx context.Context, principalID, id uuid.UUID) (*types.Chunk, error) {
	row, err := r.store.Querier(ctx).GetReadableChunk(ctx, sqlc.GetReadableChunkParams{ID: id, PrincipalID: principalID})
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrChunkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("chunk %s: %w", id, err)
	}
	return &types.Chunk{
		ID:            row.ID,
		DocumentID:    row.DocumentID,
		Seq:           int(row.Seq),
		Content:       row.Content,
		ContextHeader: row.ContextHeader,
		StartOffset:   int(row.StartOffset),
		EndOffset:     int(row.EndOffset),
		CreatedAt:     row.CreatedAt,
	}, nil
}

// DeleteChunks removes every Chunk and Parent Chunk of documentID, deleting
// the Chunks first because they carry the Parent Chunk links. It starts no
// transaction of its own: call it inside the service's ExecTx so a re-upload
// leaves the previous Chunks until its row update commits. An already empty
// Document is not an error.
func (r *chunkRepository) DeleteChunks(ctx context.Context, documentID uuid.UUID) error {
	q := r.store.Querier(ctx)
	if err := q.DeleteChunksByDocument(ctx, documentID); err != nil {
		return fmt.Errorf("delete chunks of document %s: %w", documentID, err)
	}
	if err := q.DeleteParentChunksByDocument(ctx, documentID); err != nil {
		return fmt.Errorf("delete Parent Chunks of document %s: %w", documentID, err)
	}
	return nil
}

// ReplaceChunks deletes a Document's Chunks and Parent Chunks and inserts set
// in order. It starts no transaction of its own: call it inside the service's
// ExecTx so a failed insert leaves the previous set in place. It deletes the
// Chunks before the Parent Chunks because the Chunks carry the Parent Chunk
// links, then inserts the Parent Chunks before the Chunks because the Chunks
// need their IDs; a Document re-ingested without parent-child mode keeps no
// Parent Chunk.
func (r *chunkRepository) ReplaceChunks(ctx context.Context, documentID uuid.UUID, set types.ChunkSet) error {
	if len(set.Parents) == 0 && len(set.ParentOf) != 0 {
		return fmt.Errorf("replace chunks of document %s: %d Parent Chunk links but no Parent Chunks", documentID, len(set.ParentOf))
	}
	if len(set.Parents) != 0 && len(set.ParentOf) != len(set.Chunks) {
		return fmt.Errorf("replace chunks of document %s: %d Parent Chunk links for %d Chunks",
			documentID, len(set.ParentOf), len(set.Chunks))
	}
	for i, parent := range set.ParentOf {
		if parent < 0 || parent >= len(set.Parents) {
			return fmt.Errorf("replace chunks of document %s: Chunk %d names Parent Chunk %d of %d",
				documentID, i, parent, len(set.Parents))
		}
	}

	q := r.store.Querier(ctx)
	if err := q.DeleteChunksByDocument(ctx, documentID); err != nil {
		return fmt.Errorf("delete chunks of document %s: %w", documentID, err)
	}
	if err := q.DeleteParentChunksByDocument(ctx, documentID); err != nil {
		return fmt.Errorf("delete Parent Chunks of document %s: %w", documentID, err)
	}

	parentIDs := make([]uuid.UUID, len(set.Parents))
	for i, p := range set.Parents {
		id, err := q.InsertParentChunk(ctx, sqlc.InsertParentChunkParams{
			DocumentID:    documentID,
			Seq:           int32(p.Seq),
			Content:       p.Content,
			ContextHeader: p.ContextHeader,
			StartOffset:   int32(p.StartOffset),
			EndOffset:     int32(p.EndOffset),
		})
		if err != nil {
			return fmt.Errorf("insert Parent Chunk %d of document %s: %w", p.Seq, documentID, err)
		}
		parentIDs[i] = id
	}

	for i, c := range set.Chunks {
		params := sqlc.InsertChunkParams{
			DocumentID:    documentID,
			Seq:           int32(c.Seq),
			Content:       c.Content,
			ContextHeader: c.ContextHeader,
			StartOffset:   int32(c.StartOffset),
			EndOffset:     int32(c.EndOffset),
		}
		if len(set.ParentOf) != 0 {
			params.ParentID = pgtype.UUID{Bytes: parentIDs[set.ParentOf[i]], Valid: true}
		}
		if err := q.InsertChunk(ctx, params); err != nil {
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
		matches = append(matches, newChunkMatch(row.ID, row.DocumentID, row.Seq,
			row.Content, row.ContextHeader, row.ParentID, row.Score))
	}
	return matches, nil
}

// SearchBM25 returns the Chunks of documentIDs that match query, most
// relevant by pg_search's BM25 score first, ties broken by Chunk ID. The
// readable-ID filter travels into the BM25 index query, so pg_search applies
// it before its top-K: a Chunk the caller cannot read can neither come back
// nor push a readable Chunk out of the answer (ADR-0004). The query is a
// match-style query tokenized with the field, not a Tantivy query string, so
// a caller's punctuation cannot error or change its meaning.
func (r *chunkRepository) SearchBM25(ctx context.Context, documentIDs []uuid.UUID,
	query string, topK int,
) ([]*types.ChunkMatch, error) {
	rows, err := r.store.Querier(ctx).SearchBM25Chunks(ctx, sqlc.SearchBM25ChunksParams{
		DocumentIds: documentIDs,
		Query:       query,
		TopK:        int32(topK),
	})
	if err != nil {
		return nil, fmt.Errorf("search bm25 chunks of %d documents: %w", len(documentIDs), err)
	}
	matches := make([]*types.ChunkMatch, 0, len(rows))
	for _, row := range rows {
		matches = append(matches, newChunkMatch(row.ID, row.DocumentID, row.Seq,
			row.Content, row.ContextHeader, row.ParentID, row.Score))
	}
	return matches, nil
}

// ListParentChunks returns the Parent Chunks of documentIDs whose IDs are in
// parentIDs. The document filter is part of the query, so a Parent Chunk of a
// Document the caller cannot read is never returned, even when a stale Chunk
// still links to it (ADR-0004).
func (r *chunkRepository) ListParentChunks(ctx context.Context, documentIDs, parentIDs []uuid.UUID) ([]*types.ParentChunk, error) {
	rows, err := r.store.Querier(ctx).ListParentChunks(ctx, sqlc.ListParentChunksParams{
		DocumentIds: documentIDs,
		ParentIds:   parentIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("list parent chunks of %d documents: %w", len(documentIDs), err)
	}
	parents := make([]*types.ParentChunk, 0, len(rows))
	for _, row := range rows {
		parents = append(parents, &types.ParentChunk{
			ID:            row.ID,
			DocumentID:    row.DocumentID,
			Seq:           int(row.Seq),
			Content:       row.Content,
			ContextHeader: row.ContextHeader,
			StartOffset:   int(row.StartOffset),
			EndOffset:     int(row.EndOffset),
			CreatedAt:     row.CreatedAt,
		})
	}
	return parents, nil
}

// newChunkMatch builds a match from the scalar fields the dense and BM25 rows
// share; their generated row types differ but carry the same seven columns.
func newChunkMatch(id, documentID uuid.UUID, seq int32, content, contextHeader string,
	parentID pgtype.UUID, score float64,
) *types.ChunkMatch {
	var parent *uuid.UUID
	if parentID.Valid {
		id := uuid.UUID(parentID.Bytes)
		parent = &id
	}
	return &types.ChunkMatch{
		Chunk: &types.Chunk{
			ID:            id,
			DocumentID:    documentID,
			Seq:           int(seq),
			Content:       content,
			ContextHeader: contextHeader,
		},
		ParentID: parent,
		Score:    score,
	}
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
