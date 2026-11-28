package handler

import (
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/shutdown"
)

// defaultAnswerStreamKeepalive is how long an idle answer stream waits before
// writing a comment frame, so a proxy does not close it.
const defaultAnswerStreamKeepalive = 15 * time.Second

// WithAnswerStreamKeepalive sets how long an idle answer stream waits before
// writing a keepalive comment. A non-positive duration is ignored.
func WithAnswerStreamKeepalive(d time.Duration) AnswerHandlerOption {
	return func(h *AnswerHandler) {
		if d > 0 {
			h.streamKeepalive = d
		}
	}
}

// wantsEventStream reports whether the Accept header prefers the answer as a
// Server-Sent Events stream: text/event-stream has a positive q that is at
// least the q of application/json and of */*.
func wantsEventStream(r *http.Request) bool {
	quality := make(map[string]float64)
	for _, entry := range strings.Split(r.Header.Get("Accept"), ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(entry))
		if err != nil {
			continue
		}
		mediaType = strings.ToLower(mediaType)
		q := 1.0
		if raw, ok := params["q"]; ok {
			parsed, err := strconv.ParseFloat(raw, 64)
			if err != nil || parsed < 0 || parsed > 1 {
				continue
			}
			q = parsed
		}
		if previous, ok := quality[mediaType]; !ok || q > previous {
			quality[mediaType] = q
		}
	}
	stream, ok := quality["text/event-stream"]
	if !ok || stream <= 0 {
		return false
	}
	if json, ok := quality["application/json"]; ok && stream < json {
		return false
	}
	if any, ok := quality["*/*"]; ok && stream < any {
		return false
	}
	return true
}

// StreamAnswerEvents answers GET /answers/{answer_id}/events with the events
// of an answer started by POST /answer with Accept: text/event-stream. It
// checks that the caller is the Principal who asked and replays only the
// events after Last-Event-ID before carrying on live. Any other Principal
// gets the same 404 as for an unknown or expired answer ID.
//
//	@Summary		Resume a streamed Pipeline Answer
//	@Description	Streams a Pipeline Answer's Answer Events as Server-Sent Events, starting after Last-Event-ID. Each frame's id is the event's seq within the answer and event is started, token, citation, done or error. started carries {"answer_id"}, token {"text"} (the raw model delta), citation {"marker","chunk_id","document_id"} once per cited source in first-appearance order, and done {"answer","finish_reason","debug"?} or error {"message"} ends the stream. Only the Principal who asked for the answer may resume it; any other caller gets the same 404 as for an unknown or expired answer ID.
//	@Tags			answering
//	@Produce		text/event-stream
//	@Security		bearerAuth
//	@Param			answer_id		path	string	true	"Answer ID"	format(uuid)
//	@Param			Last-Event-ID	header	string	false	"Seq of the last event the client saw; only later events are sent"
//	@Success		200				{string}	string	"Server-Sent Events, one frame per answer event; the stream ends after done or error"
//	@Failure		400				{object}	handler.ErrorResponse	"Invalid Last-Event-ID"
//	@Failure		401				{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		404				{object}	handler.ErrorResponse	"No answer stream with that ID, it expired, or it belongs to another Principal"
//	@Failure		500				{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Router			/answers/{answer_id}/events [get]
func (h *AnswerHandler) StreamAnswerEvents(w http.ResponseWriter, r *http.Request) {
	answerID, idErr := uuid.Parse(chi.URLParam(r, "answer_id"))
	after, ok := parseLastEventID(r.Header.Get("Last-Event-ID"))
	if !ok {
		writeError(w, r, errors.NewBadRequestError("Invalid Last-Event-ID: must be a non-negative integer"))
		return
	}
	if idErr != nil {
		// An ID that is not a UUID names no answer. The zero UUID reaches the
		// service's one not-found error, so a malformed, unknown and foreign
		// ID answer the same body.
		answerID = uuid.Nil
	}
	h.streamAnswerEvents(w, r, answerID, after)
}

// streamAnswerEvents subscribes from after, writes the SSE headers and streams
// every event until the answer ends, the client leaves, or the server shuts
// down; while idle it writes a keepalive comment. The first subscribe runs
// before any SSE header, so its not-found error reaches the client as the
// ordinary JSON 404.
func (h *AnswerHandler) streamAnswerEvents(w http.ResponseWriter, r *http.Request, answerID uuid.UUID, after int64) {
	ctx := r.Context()
	subscription, err := h.service.AnswerEvents(ctx, answerID, after)
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
		// A client that disconnected before the headers reached it lands
		// here; there is no one to report it to.
		return
	}

	keepalive := h.streamKeepalive
	if keepalive <= 0 {
		keepalive = defaultAnswerStreamKeepalive
	}
	idle := time.NewTimer(keepalive)
	defer idle.Stop()
	for {
		for _, event := range subscription.Events {
			if err := stream.Event(event.Seq, string(event.Type), event.Data); err != nil {
				// The client went away; there is no one to report it to.
				return
			}
			after = event.Seq
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(keepalive)
		}
		if subscription.Done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-shutdown.Signal(ctx):
			return
		case <-idle.C:
			if err := stream.Comment("keepalive"); err != nil {
				return
			}
			idle.Reset(keepalive)
		case <-subscription.Wait:
			subscription, err = h.service.AnswerEvents(ctx, answerID, after)
			if err != nil {
				// The answer expired or stopped being readable while the
				// client waited; the stream ends quietly, as it would on a
				// dropped connection.
				return
			}
		}
	}
}
