package interfaces

import (
	"context"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types"
)

// IngestEventRepository persists the Ingest Events of Documents. A client
// replays them over the SSE stream of GET /documents/{id}/events.
type IngestEventRepository interface {
	// InsertIngestEvent appends event, carrying its UploadID, to its Document's
	// stream and returns it with the Document's next Seq and CreatedAt
	// Postgres assigned. It starts no transaction: an event that claims a
	// step finished is inserted in the transaction that stores that step's
	// data, so it appears only when the data does. The insert advances a
	// per-Document counter under the Document's row lock, so Seq order is
	// commit order and a reader that resumes after a Seq cannot miss an
	// earlier event that commits later. ErrDocumentNotFound means the
	// Document is gone.
	InsertIngestEvent(ctx context.Context, event *types.IngestEvent) (*types.IngestEvent, error)
	// ListIngestEventsAfter returns the Document's events with Seq greater
	// than afterSeq, in Seq order. A Document with no events gives an empty
	// list, not an error.
	ListIngestEventsAfter(ctx context.Context, documentID uuid.UUID, afterSeq int64) ([]*types.IngestEvent, error)
}
