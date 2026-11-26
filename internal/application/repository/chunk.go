package repository

import (
	"context"
	"fmt"

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
