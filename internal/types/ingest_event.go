package types

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Ingest Event types: the steps of a Document's ingest a client can follow
// over the SSE stream of GET /documents/{id}/events. A retried activity can
// write a second *_finished event for work an earlier attempt already
// committed; that is accepted, and clients treat the events as a stream, not
// as a count of attempts.
const (
	// IngestEventParseStarted means ParseAndChunk is starting. Detail is a
	// ParseStartedDetail.
	IngestEventParseStarted = "parse_started"
	// IngestEventParseFinished means the Chunks were committed. Detail is a
	// ParseFinishedDetail.
	IngestEventParseFinished = "parse_finished"
	// IngestEventEmbedBatchFinished means one embedding batch was stored.
	// Detail is an EmbedBatchFinishedDetail.
	IngestEventEmbedBatchFinished = "embed_batch_finished"
	// IngestEventCompleted means the Document is completed. Detail is empty,
	// and no event follows it on the stream.
	IngestEventCompleted = "completed"
	// IngestEventFailed means the Document is failed. Detail is a
	// FailedDetail, and no event follows it on the stream.
	IngestEventFailed = "failed"
)

// IngestEvent is one step of a Document's ingest, recorded in Postgres so a
// client can follow the progress as a SSE stream and resume from the last Seq
// it saw (#23). Detail carries IDs and counts only: never a file name, Chunk
// text or raw error text.
type IngestEvent struct {
	// Seq is the Document's own sequence number, starting at 1. The insert
	// advances a per-Document counter under the Document's row lock, so Seq
	// order is commit order and a client can resume safely after one. It is
	// the stream's event id and the resume cursor, so it is not repeated in
	// the data line.
	Seq int64 `json:"-"`
	// DocumentID is the Document whose ingest the event belongs to.
	DocumentID uuid.UUID `json:"document_id"`
	// Type is one of the IngestEvent* constants.
	Type string `json:"type"`
	// Detail is the JSON object type describes.
	Detail json.RawMessage `json:"detail"`
	// CreatedAt is when the event was committed.
	CreatedAt time.Time `json:"created_at"`
}

// ParseStartedDetail is the detail of an IngestEventParseStarted event.
type ParseStartedDetail struct {
	// Attempt is the Temporal activity attempt that is starting, from 1.
	Attempt int `json:"attempt"`
}

// ParseFinishedDetail is the detail of an IngestEventParseFinished event.
type ParseFinishedDetail struct {
	// Chunks is how many Chunks the attempt committed.
	Chunks int `json:"chunks"`
}

// EmbedBatchFinishedDetail is the detail of an IngestEventEmbedBatchFinished
// event. A batch with nothing left to embed writes no event.
type EmbedBatchFinishedDetail struct {
	// From is the first Chunk seq of the batch, inclusive.
	From int `json:"from"`
	// To is the last Chunk seq of the batch, exclusive.
	To int `json:"to"`
	// Chunks is how many Chunks the batch embedded and stored.
	Chunks int `json:"chunks"`
}

// FailedDetail is the detail of an IngestEventFailed event.
type FailedDetail struct {
	// Reason is the stored parse_error: short, and chosen to be safe to show.
	Reason string `json:"reason"`
}

// IsTerminal reports whether the event ends its Document's stream: completed
// or failed. The stream writes no event after a terminal one.
func (e IngestEvent) IsTerminal() bool {
	return e.Type == IngestEventCompleted || e.Type == IngestEventFailed
}
