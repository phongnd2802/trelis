package service

import (
	"testing"
	"time"

	"github.com/google/uuid"

	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
)

// hubEvents returns the Answer Events a subscriber sees after afterSeq.
func hubEvents(t *testing.T, h *answerStreamHub, id, owner uuid.UUID, afterSeq int64) []types.AnswerEvent {
	t.Helper()
	subscription, err := h.subscribe(id, owner, afterSeq)
	if err != nil {
		t.Fatalf("subscribe after %d: %v", afterSeq, err)
	}
	return subscription.Events
}

func TestAnswerStreamHubNumbersAndReplaysEvents(t *testing.T) {
	h := newAnswerStreamHub(time.Hour, 100)
	owner := uuid.New()
	id := h.start(owner)
	if !h.append(id, types.AnswerEventToken, types.AnswerTokenEvent{Text: "Alpha "}) {
		t.Fatal("append token = false, want the event stored")
	}
	if !h.append(id, types.AnswerEventDone, types.AnswerDoneEvent{Answer: "Alpha"}) {
		t.Fatal("append done = false, want the event stored")
	}
	if subscription, err := h.subscribe(id, owner, 0); err != nil || !subscription.Done {
		t.Fatalf("subscribe after done = %+v, %v; want the stream ended", subscription, err)
	}

	all := hubEvents(t, h, id, owner, 0)
	if len(all) != 3 {
		t.Fatalf("events = %+v, want started, token and done", all)
	}
	for i, event := range all {
		if event.Seq != int64(i+1) {
			t.Errorf("event %d seq = %d, want %d", i, event.Seq, i+1)
		}
	}
	if all[0].Type != types.AnswerEventStarted {
		t.Errorf("event 0 type = %q, want %q", all[0].Type, types.AnswerEventStarted)
	}
	if started, ok := all[0].Data.(types.AnswerStartedEvent); !ok || started.AnswerID != id {
		t.Errorf("started data = %+v, want the answer ID %s", all[0].Data, id)
	}
	if token, ok := all[1].Data.(types.AnswerTokenEvent); !ok || token.Text != "Alpha " {
		t.Errorf("token data = %+v, want the raw delta", all[1].Data)
	}

	// Resuming after seq 1 replays only the later events.
	later := hubEvents(t, h, id, owner, 1)
	if len(later) != 2 || later[0].Seq != 2 || later[1].Seq != 3 {
		t.Errorf("events after seq 1 = %+v, want seq 2 and 3", later)
	}
}

func TestAnswerStreamHubWakesAWaiterOnAppend(t *testing.T) {
	h := newAnswerStreamHub(time.Hour, 100)
	owner := uuid.New()
	id := h.start(owner)

	subscription, err := h.subscribe(id, owner, 1)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if subscription.Done {
		t.Fatal("done = true before the stream finished")
	}
	select {
	case <-subscription.Wait:
		t.Fatal("the waiter woke before anything was appended")
	default:
	}

	h.append(id, types.AnswerEventToken, types.AnswerTokenEvent{Text: "more"})
	select {
	case <-subscription.Wait:
	case <-time.After(time.Second):
		t.Fatal("the waiter did not wake on append")
	}
	if events := hubEvents(t, h, id, owner, 1); len(events) != 1 {
		t.Errorf("events after seq 1 = %+v, want the appended token", events)
	}

	subscription, err = h.subscribe(id, owner, 2)
	if err != nil {
		t.Fatalf("subscribe after token: %v", err)
	}
	h.finish(id)
	select {
	case <-subscription.Wait:
	case <-time.After(time.Second):
		t.Fatal("the waiter did not wake on finish")
	}
	subscription, err = h.subscribe(id, owner, 2)
	if err != nil {
		t.Fatalf("subscribe after finish: %v", err)
	}
	if !subscription.Done {
		t.Error("done = false after finish, want true")
	}
}

func TestAnswerStreamHubHidesAnotherOwnersStream(t *testing.T) {
	h := newAnswerStreamHub(time.Hour, 100)
	answerID := h.start(uuid.New())

	_, unreadable := h.subscribe(answerID, uuid.New(), 0)
	_, missing := h.subscribe(uuid.New(), uuid.New(), 0)
	unreadableErr, ok := apperrors.IsAppError(unreadable)
	if !ok || unreadableErr.HTTPCode != 404 {
		t.Fatalf("another owner's subscribe = %v, want the not-found error", unreadable)
	}
	missingErr, ok := apperrors.IsAppError(missing)
	if !ok || missingErr.HTTPCode != 404 {
		t.Fatalf("unknown answer subscribe = %v, want the not-found error", missing)
	}
	if unreadableErr.Message != missingErr.Message {
		t.Errorf("unreadable message %q differs from unknown message %q, which reveals existence",
			unreadableErr.Message, missingErr.Message)
	}
}

func TestAnswerStreamHubEvictsAFinishedStreamAfterRetention(t *testing.T) {
	h := newAnswerStreamHub(10*time.Millisecond, 100)
	owner := uuid.New()
	id := h.start(owner)
	h.finish(id)

	deadline := time.Now().Add(time.Second)
	for {
		if _, err := h.subscribe(id, owner, 0); err != nil {
			appErr, ok := apperrors.IsAppError(err)
			if !ok || appErr.HTTPCode != 404 {
				t.Fatalf("subscribe after eviction = %v, want the not-found error", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the finished stream was not evicted after its retention")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAnswerStreamHubEndsAtTheEventCap(t *testing.T) {
	h := newAnswerStreamHub(time.Hour, 3)
	owner := uuid.New()
	id := h.start(owner)
	if !h.append(id, types.AnswerEventToken, types.AnswerTokenEvent{Text: "one"}) {
		t.Fatal("append within the cap = false, want the token stored")
	}
	if h.append(id, types.AnswerEventToken, types.AnswerTokenEvent{Text: "two"}) {
		t.Fatal("append beyond the cap = true, want the stream ended instead")
	}

	subscription, err := h.subscribe(id, owner, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if !subscription.Done {
		t.Error("done = false at the cap, want the stream ended")
	}
	if len(subscription.Events) != 3 {
		t.Fatalf("events = %+v, want the cap of 3 events kept", subscription.Events)
	}
	last := subscription.Events[2]
	if last.Type != types.AnswerEventError {
		t.Errorf("last event = %q, want %q", last.Type, types.AnswerEventError)
	}
	if failure, ok := last.Data.(types.AnswerErrorEvent); !ok || failure.Message == "" {
		t.Errorf("last data = %+v, want a generic error message", last.Data)
	}
	if h.append(id, types.AnswerEventDone, types.AnswerDoneEvent{Answer: "late"}) {
		t.Error("append after the cap = true, want it refused")
	}
}
