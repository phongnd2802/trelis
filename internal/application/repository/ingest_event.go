package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/sqlc"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

type ingestEventRepository struct {
	store db.Store
}

// NewIngestEventRepository returns an IngestEventRepository on store.
func NewIngestEventRepository(store db.Store) interfaces.IngestEventRepository {
	return &ingestEventRepository{store: store}
}

// InsertIngestEvent appends event to its Document's stream and returns it with
// the Document's next seq and Postgres' created_at. The insert advances the
// Document's ingest_event_seq under its row lock; a caller runs it first in a
// transaction that also writes Chunk rows, so locks are taken Document first,
// Chunks second, the same order the delete cascade uses, and the two cannot
// deadlock. A Document with no row left is ErrDocumentNotFound.
func (r *ingestEventRepository) InsertIngestEvent(ctx context.Context, event *types.IngestEvent) (*types.IngestEvent, error) {
	row, err := r.store.Querier(ctx).InsertIngestEvent(ctx, sqlc.InsertIngestEventParams{
		DocumentID: event.DocumentID,
		Type:       event.Type,
		Detail:     event.Detail,
	})
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrDocumentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("insert %s event of document %s: %w", event.Type, event.DocumentID, err)
	}
	return fromIngestEventRow(row), nil
}

// ListIngestEventsAfter returns the Document's events after afterSeq, in order.
func (r *ingestEventRepository) ListIngestEventsAfter(ctx context.Context, documentID uuid.UUID, afterSeq int64) ([]*types.IngestEvent, error) {
	rows, err := r.store.Querier(ctx).ListIngestEventsAfter(ctx, sqlc.ListIngestEventsAfterParams{
		DocumentID: documentID,
		AfterSeq:   afterSeq,
	})
	if err != nil {
		return nil, fmt.Errorf("list ingest events of document %s after %d: %w", documentID, afterSeq, err)
	}
	events := make([]*types.IngestEvent, len(rows))
	for i, row := range rows {
		events[i] = fromIngestEventRow(row)
	}
	return events, nil
}

func fromIngestEventRow(row sqlc.IngestEvent) *types.IngestEvent {
	return &types.IngestEvent{
		Seq:        row.Seq,
		DocumentID: row.DocumentID,
		Type:       row.Type,
		Detail:     json.RawMessage(row.Detail),
		CreatedAt:  row.CreatedAt,
	}
}
