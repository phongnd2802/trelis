package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/shutdown"
)

const (
	// defaultIngestEventPollInterval is how often an open progress stream
	// looks for new Ingest Events; 500ms keeps the stream prompt without
	// polling Postgres hard per subscriber.
	defaultIngestEventPollInterval = 500 * time.Millisecond
	// defaultIngestEventKeepalive is how long an idle stream waits before
	// writing a comment frame, so a proxy does not close it.
	defaultIngestEventKeepalive = 15 * time.Second
	// defaultIngestEventTimeout caps one stream, so a Document stuck in
	// processing (for example after MarkFailed ran out of retries) cannot hold
	// a subscriber forever. The client reconnects with Last-Event-ID and loses
	// nothing.
	defaultIngestEventTimeout = 30 * time.Minute
)

// StreamIngestEvents answers GET /documents/{id}/events with a Server-Sent
// Events stream of the Document's ingest progress. It checks that the caller
// can read the Document before writing any SSE header, then polls for new
// events and re-checks access on every poll, so a revoked grant or a deleted
// Document ends the stream. It ends after the completed or failed event, when
// the client disconnects, when the Document is gone or no longer readable,
// when the server shuts down, or after 30 minutes; a client that reconnects
// with Last-Event-ID loses nothing.
//
//	@Summary		Stream a Document's ingest progress
//	@Description	Streams the Document's Ingest Events as Server-Sent Events while the caller can read it. Each frame's id is the event's seq within the Document, event is parse_started, parse_finished, embed_batch_finished, upload_replaced, completed or failed, and data is {"document_id","upload_id","type","detail","created_at"}. detail is {"attempt"} for parse_started, {"chunks"} for parse_finished, {"from","to","chunks"} for embed_batch_finished, {"upload_id"} for upload_replaced, {"reason"} for failed, and {} for completed. Reconnecting with Last-Event-ID replays the events after that seq, then carries on live. After a re-upload the stream passes an older upload's completed or failed event, then its upload_replaced event, and ends at the current upload's completed or failed. It also ends when the Document is gone or no longer readable, when the client disconnects, when the server shuts down, or after 30 minutes so a Document stuck in processing cannot hold it forever; reconnecting with Last-Event-ID loses nothing.
//	@Tags			documents
//	@Produce		text/event-stream
//	@Security		bearerAuth
//	@Param			id				path	string	true	"Document ID"	format(uuid)
//	@Param			Last-Event-ID	header	string	false	"Per-Document seq of the last event the client saw; only later events are sent"
//	@Success		200				{string}	string	"Server-Sent Events, one frame per Ingest Event; the stream ends after the current upload's completed or failed, or after 30 minutes"
//	@Failure		400				{object}	handler.ErrorResponse	"Invalid Document ID or Last-Event-ID"
//	@Failure		401				{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		404				{object}	handler.ErrorResponse	"No readable Document with that ID"
//	@Failure		500				{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Router			/documents/{id}/events [get]
func (h *DocumentHandler) StreamIngestEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, errors.NewBadRequestError("Invalid Document ID"))
		return
	}
	after, ok := parseLastEventID(r.Header.Get("Last-Event-ID"))
	if !ok {
		writeError(w, r, errors.NewBadRequestError("Invalid Last-Event-ID: must be a non-negative integer"))
		return
	}

	// The first read also decides access (ADR-0004): a caller who cannot read
	// the Document gets the ordinary JSON 404 before any SSE header.
	events, currentUpload, err := h.service.IngestEventsAfter(ctx, id, after)
	if err != nil {
		writeError(w, r, err)
		return
	}

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	stream := NewSSEWriter(w)
	if err := stream.Flush(); err != nil {
		// A client that disconnected before the headers reached it lands here.
		// That is not a server failure, so it is not logged; the request's
		// access line still records the response.
		return
	}

	interval := h.ingestEventPollInterval
	if interval <= 0 {
		interval = defaultIngestEventPollInterval
	}
	keepalive := h.ingestEventKeepalive
	if keepalive <= 0 {
		keepalive = defaultIngestEventKeepalive
	}
	timeout := h.ingestEventTimeout
	if timeout <= 0 {
		timeout = defaultIngestEventTimeout
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	lastFrame := time.Now()
	for {
		for _, event := range events {
			if err := stream.Event(event.Seq, event.Type, event); err != nil {
				// The client went away; there is no one to report it to.
				return
			}
			after = event.Seq
			lastFrame = time.Now()
			if event.IsTerminalFor(currentUpload) {
				// An older upload's completed or failed event is followed by
				// upload_replaced and the new upload's events; only the
				// current upload's terminal event ends the stream.
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-shutdown.Signal(ctx):
			return
		case <-deadline.C:
			// The cap expired, probably on a Document stuck in processing. End
			// quietly; Last-Event-ID resumes from where this stream stopped.
			return
		case <-ticker.C:
		}
		if time.Since(lastFrame) >= keepalive {
			if err := stream.Comment("keepalive"); err != nil {
				return
			}
			lastFrame = time.Now()
		}
		events, currentUpload, err = h.service.IngestEventsAfter(ctx, id, after)
		if err != nil {
			if appErr, isApp := errors.IsAppError(err); isApp && appErr.HTTPCode == http.StatusNotFound {
				// The Document is gone or no longer readable: end quietly.
				return
			}
			logger.Ctx(ctx).Error("stream ingest events: poll", zap.Stringer("document", id), zap.Error(err))
			return
		}
	}
}

// parseLastEventID reads the Last-Event-ID header. Absent means 0, so the
// stream starts at the first event; a value that is not a non-negative
// integer is a client mistake.
func parseLastEventID(raw string) (int64, bool) {
	if raw == "" {
		return 0, true
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq < 0 {
		return 0, false
	}
	return seq, true
}
