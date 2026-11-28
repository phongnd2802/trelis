package service

import (
	"sync"
	"time"

	"github.com/google/uuid"

	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
)

const (
	// answerStreamRetention is how long a finished answer's events stay
	// resumable. The client's generation has already ended, so keeping them
	// longer only spends memory.
	answerStreamRetention = 10 * time.Minute
	// answerStreamMaxEvents caps the events kept for one answer, terminal
	// event included, so one pathological answer cannot grow without bound.
	// Reaching it ends the stream with a generic error event a client can
	// retry from.
	answerStreamMaxEvents = 10_000
	// answerStreamCapMessage is the client-facing message of the error event
	// that ends a stream at the event cap.
	answerStreamCapMessage = "the answer is too long to keep for resuming; ask again"
)

// answerStreamNotFound is the one not-found error for an unknown, expired or
// another Principal's answer, so a caller cannot tell them apart.
var answerStreamNotFound = apperrors.NewNotFoundError("Answer stream not found")

// answerStreamHub holds the buffered events of the answers this process is
// streaming (ADR-0010). A live subscriber and a resuming one read the same
// buffer, so a dropped client loses nothing while the answer is retained.
type answerStreamHub struct {
	mu      sync.Mutex
	streams map[uuid.UUID]*answerStream
	// retention is how long a finished stream stays resumable.
	retention time.Duration
	// maxEvents is the most events one stream keeps, terminal event included.
	maxEvents int
}

// answerStream is one answer's buffered events and the channel waiters sleep
// on. wait is closed and replaced whenever events are appended or the stream
// finishes.
type answerStream struct {
	id     uuid.UUID
	owner  uuid.UUID
	events []types.AnswerEvent
	done   bool
	wait   chan struct{}
}

// newAnswerStreamHub returns a hub retaining finished streams for retention
// and keeping at most maxEvents events per stream. Non-positive values take
// the package defaults; tests pass short values.
func newAnswerStreamHub(retention time.Duration, maxEvents int) *answerStreamHub {
	if retention <= 0 {
		retention = answerStreamRetention
	}
	if maxEvents < 2 { // room for started and a terminal event
		maxEvents = answerStreamMaxEvents
	}
	return &answerStreamHub{
		streams:   make(map[uuid.UUID]*answerStream),
		retention: retention,
		maxEvents: maxEvents,
	}
}

// start creates a stream owned by owner, appends its started event and
// returns its ID.
func (h *answerStreamHub) start(owner uuid.UUID) uuid.UUID {
	id := uuid.New()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.streams[id] = &answerStream{
		id:     id,
		owner:  owner,
		wait:   make(chan struct{}),
		events: []types.AnswerEvent{{Seq: 1, Type: types.AnswerEventStarted, Data: types.AnswerStartedEvent{AnswerID: id}}},
	}
	return id
}

// append stores event with the stream's next seq and wakes its waiters. A
// terminal event (done or error) also finishes the stream. It returns false
// when the stream already ended or the event cap was reached; at the cap it
// stores a terminal error event instead and finishes the stream, so an append
// never grows a stream past maxEvents.
func (h *answerStreamHub) append(id uuid.UUID, eventType types.AnswerEventType, data any) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	stream, ok := h.streams[id]
	if !ok || stream.done {
		return false
	}
	terminal := eventType == types.AnswerEventDone || eventType == types.AnswerEventError
	if !terminal && len(stream.events) >= h.maxEvents-1 {
		stream.events = append(stream.events, types.AnswerEvent{
			Seq:  int64(len(stream.events) + 1),
			Type: types.AnswerEventError,
			Data: types.AnswerErrorEvent{Message: answerStreamCapMessage},
		})
		h.finishLocked(stream)
		return false
	}
	if len(stream.events) >= h.maxEvents {
		return false
	}
	stream.events = append(stream.events, types.AnswerEvent{
		Seq:  int64(len(stream.events) + 1),
		Type: eventType,
		Data: data,
	})
	if terminal {
		h.finishLocked(stream)
		return true
	}
	h.wakeLocked(stream)
	return true
}

// finish marks the stream ended, wakes every waiter and starts its retention
// timer. Finishing twice does nothing.
func (h *answerStreamHub) finish(id uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if stream, ok := h.streams[id]; ok {
		h.finishLocked(stream)
	}
}

// finishLocked is finish for a caller already holding the lock.
func (h *answerStreamHub) finishLocked(stream *answerStream) {
	if stream.done {
		return
	}
	stream.done = true
	close(stream.wait)
	time.AfterFunc(h.retention, func() { h.evict(stream.id, stream) })
}

// wakeLocked closes the current wait channel and replaces it, so a waiter
// that wakes sees the new events on its next subscribe.
func (h *answerStreamHub) wakeLocked(stream *answerStream) {
	close(stream.wait)
	stream.wait = make(chan struct{})
}

// evict drops a finished stream once its retention passes. It deletes the
// entry only if it is still the stream the timer was started for.
func (h *answerStreamHub) evict(id uuid.UUID, stream *answerStream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams[id] == stream {
		delete(h.streams, id)
	}
}

// subscribe returns the Answer Events after afterSeq, a channel closed when
// more arrive or the stream ends, and whether it has ended. An unknown answer
// and another Principal's answer give the same not-found error.
func (h *answerStreamHub) subscribe(id, owner uuid.UUID, afterSeq int64) (types.AnswerSubscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	stream, ok := h.streams[id]
	if !ok || stream.owner != owner {
		return types.AnswerSubscription{}, answerStreamNotFound
	}
	var events []types.AnswerEvent
	for _, event := range stream.events {
		if event.Seq > afterSeq {
			events = append(events, event)
		}
	}
	return types.AnswerSubscription{Events: events, Wait: stream.wait, Done: stream.done}, nil
}
