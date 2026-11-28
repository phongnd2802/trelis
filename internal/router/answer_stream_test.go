package router

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/service"
	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/handler"
	"github.com/phongnd2802/trelis/internal/types"
)

// stubStreamGenerator streams a fixed chunk sequence for the router tests.
type stubStreamGenerator struct {
	chunks []types.StreamResponse
}

func (g *stubStreamGenerator) Generate(context.Context, []types.ChatTurn) (*types.ChatResponse, error) {
	return nil, nil
}

func (g *stubStreamGenerator) GenerateStream(context.Context, []types.ChatTurn) (<-chan types.StreamResponse, error) {
	ch := make(chan types.StreamResponse, len(g.chunks))
	for _, chunk := range g.chunks {
		ch <- chunk
	}
	close(ch)
	return ch, nil
}

func (g *stubStreamGenerator) GetModelName() string { return "test-chat" }

// postAnswerWithAccept sends body to POST /answer with the given Accept
// header.
func postAnswerWithAccept(t *testing.T, h http.Handler, body, accept, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/answer", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// getAnswerEvents sends GET /answers/{id}/events with the given
// Last-Event-ID.
func getAnswerEvents(t *testing.T, h http.Handler, answerID, lastEventID, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/answers/"+answerID+"/events", nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// eventPayload decodes a frame's data as T.
func eventPayload[T any](t *testing.T, frame sseFrame) T {
	t.Helper()
	var payload T
	if err := json.Unmarshal([]byte(frame.data), &payload); err != nil {
		t.Fatalf("data %q is not the %T payload: %v", frame.data, payload, err)
	}
	return payload
}

// answerStreamEvents is a finished answer's event list for the stream tests.
func answerStreamEvents(answerID uuid.UUID) ([]types.AnswerEvent, uuid.UUID, uuid.UUID) {
	chunkID := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	documentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	return []types.AnswerEvent{
		{Seq: 1, Type: types.AnswerEventStarted, Data: types.AnswerStartedEvent{AnswerID: answerID}},
		{Seq: 2, Type: types.AnswerEventToken, Data: types.AnswerTokenEvent{Text: "Alpha "}},
		{Seq: 3, Type: types.AnswerEventToken, Data: types.AnswerTokenEvent{Text: "[1]."}},
		{Seq: 4, Type: types.AnswerEventCitation, Data: types.AnswerCitationEvent{
			Marker: 1, ChunkID: chunkID, DocumentID: documentID,
		}},
		{Seq: 5, Type: types.AnswerEventDone, Data: types.AnswerDoneEvent{
			Answer: "Alpha [1].", FinishReason: "stop",
		}},
	}, chunkID, documentID
}

func TestAnswerStreamsWhenAcceptAsksForIt(t *testing.T) {
	answerID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000aa")
	events, chunkID, documentID := answerStreamEvents(answerID)
	svc := &stubAnswer{streamID: answerID, events: events, streamDone: true}
	h := newAnswerRouter(svc)

	rec := postAnswerWithAccept(t, h, `{"query":"who won?","debug":true}`,
		"text/event-stream", "Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /answer with Accept: text/event-stream = %d %s, want 200", rec.Code, rec.Body)
	}
	for header, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if svc.startCalls != 1 || svc.calls != 0 {
		t.Errorf("service calls = %d start and %d JSON, want one start call", svc.startCalls, svc.calls)
	}
	if svc.gotStream == nil || svc.gotStream.Query != "who won?" || !svc.gotStream.Debug {
		t.Errorf("start request = %+v, want the decoded query with debug", svc.gotStream)
	}
	if svc.afterSeqs[0] != 0 {
		t.Errorf("first subscribe after = %d, want 0 so the stream starts at started", svc.afterSeqs[0])
	}

	frames := parseSSEFrames(t, rec.Body.String())
	if len(frames) != len(events) {
		t.Fatalf("streamed %d frames, want %d:\n%s", len(frames), len(events), rec.Body)
	}
	for i, frame := range frames {
		if frame.id != events[i].Seq || frame.event != string(events[i].Type) {
			t.Errorf("frame %d = %d %s, want %d %s", i, frame.id, frame.event, events[i].Seq, events[i].Type)
		}
	}
	if started := eventPayload[types.AnswerStartedEvent](t, frames[0]); started.AnswerID != answerID {
		t.Errorf("started answer_id = %s, want %s", started.AnswerID, answerID)
	}
	if token := eventPayload[types.AnswerTokenEvent](t, frames[1]); token.Text != "Alpha " {
		t.Errorf("token 1 = %q, want the raw delta", token.Text)
	}
	citation := eventPayload[types.AnswerCitationEvent](t, frames[3])
	if citation.Marker != 1 || citation.ChunkID != chunkID || citation.DocumentID != documentID {
		t.Errorf("citation = %+v, want source 1 of the retrieved Chunk", citation)
	}
	done := eventPayload[types.AnswerDoneEvent](t, frames[4])
	if done.Answer != "Alpha [1]." || done.FinishReason != "stop" {
		t.Errorf("done = %+v, want the cleaned answer and finish reason", done)
	}
}

func TestAnswerWithoutTheAcceptHeaderKeepsTheJSONEnvelope(t *testing.T) {
	for _, accept := range []string{"", "application/json", "*/*"} {
		svc := &stubAnswer{result: &types.AnswerResult{Answer: "yes", Citations: []types.Citation{}}}
		rec := postAnswerWithAccept(t, newAnswerRouter(svc), `{"query":"who won?"}`, accept, "Bearer trelis_alice")
		if rec.Code != http.StatusOK {
			t.Errorf("Accept %q: POST /answer = %d %s, want 200", accept, rec.Code, rec.Body)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Accept %q: Content-Type = %q, want JSON", accept, ct)
		}
		if svc.calls != 1 || svc.startCalls != 0 {
			t.Errorf("Accept %q: service calls = %d JSON and %d start, want the JSON path", accept, svc.calls, svc.startCalls)
		}
	}
}

func TestAnswerStreamPreGenerationErrorsAreJSON(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		status   int
		wantCode int
	}{
		"validation":          {apperrors.NewValidationError("query is required"), http.StatusBadRequest, int(apperrors.ErrValidation)},
		"chat not configured": {apperrors.NewServiceUnavailableError("pipeline answers need models.chat"), http.StatusServiceUnavailable, int(apperrors.ErrServiceUnavailable)},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &stubAnswer{startErr: tc.err}
			rec := postAnswerWithAccept(t, newAnswerRouter(svc), `{"query":"who won?"}`,
				"text/event-stream", "Bearer trelis_alice")
			if rec.Code != tc.status {
				t.Fatalf("POST /answer = %d %s, want %d", rec.Code, rec.Body, tc.status)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want the JSON error, not an SSE stream", ct)
			}
			if rec.Header().Get("X-Accel-Buffering") != "" {
				t.Error("the error carries an SSE header, want none")
			}
			if e := decode(t, rec); e.Success || e.Error == nil || e.Error.Code != tc.wantCode {
				t.Errorf("body = %s, want the %d error envelope", rec.Body, tc.wantCode)
			}
		})
	}
}

func TestResumeAnswerStreamReplaysOnlyLaterEvents(t *testing.T) {
	answerID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000aa")
	events, _, _ := answerStreamEvents(answerID)
	svc := &stubAnswer{streamID: answerID, events: events, streamDone: true}
	h := newAnswerRouter(svc)

	rec := getAnswerEvents(t, h, answerID.String(), "2", "Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET answer events = %d %s, want 200", rec.Code, rec.Body)
	}
	if len(svc.afterSeqs) == 0 || svc.afterSeqs[0] != 2 {
		t.Errorf("first subscribe after = %v, want 2 from Last-Event-ID", svc.afterSeqs)
	}
	frames := parseSSEFrames(t, rec.Body.String())
	if len(frames) != 3 {
		t.Fatalf("streamed %d frames after seq 2, want 3:\n%s", len(frames), rec.Body)
	}
	for i, frame := range frames {
		if want := events[i+2]; frame.id != want.Seq || frame.event != string(want.Type) {
			t.Errorf("frame %d = %d %s, want %d %s", i, frame.id, frame.event, want.Seq, want.Type)
		}
	}
}

func TestAnotherPrincipalGetsTheSameAnswerStreamNotFound(t *testing.T) {
	bob := types.Principal{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000002"), Name: "bob"}
	svc := service.NewAnswerService(
		&stubRetrieval{result: &types.RetrievalResult{Results: []types.RetrievedChunk{{Content: "Alpha."}}}},
		&stubStreamGenerator{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: "Alpha.", Done: true, FinishReason: "stop"},
		}},
	)
	aliceCtx := types.WithPrincipal(context.Background(), alice)
	answerID, err := svc.StartAnswerStream(aliceCtx, &types.AnswerRequest{
		RetrievalRequest: types.RetrievalRequest{Query: "who won?"},
	})
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	h := newAnswerRouterWith(keyring{"trelis_alice": alice, "trelis_bob": bob}, svc)

	unreadable := getAnswerEvents(t, h, answerID.String(), "", "Bearer trelis_bob")
	missing := getAnswerEvents(t, h, uuid.NewString(), "", "Bearer trelis_bob")
	malformed := getAnswerEvents(t, h, "not-a-uuid", "", "Bearer trelis_bob")
	if unreadable.Code != http.StatusNotFound || missing.Code != http.StatusNotFound ||
		malformed.Code != http.StatusNotFound {
		t.Fatalf("bob GET answer events = %d, unknown = %d, malformed = %d; want 404 for all",
			unreadable.Code, missing.Code, malformed.Code)
	}
	if unreadable.Body.String() != missing.Body.String() || malformed.Body.String() != missing.Body.String() {
		t.Errorf("unreadable %s, malformed %s and unknown %s bodies differ, which reveals existence",
			unreadable.Body, malformed.Body, missing.Body)
	}
	if ct := unreadable.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want the JSON error, not an SSE stream", ct)
	}
	if unreadable.Header().Get("X-Accel-Buffering") != "" {
		t.Error("the 404 carries SSE headers, want none")
	}

	aliceRec := getAnswerEvents(t, h, answerID.String(), "", "Bearer trelis_alice")
	if aliceRec.Code != http.StatusOK {
		t.Fatalf("alice GET her answer events = %d %s, want 200", aliceRec.Code, aliceRec.Body)
	}
	if frames := parseSSEFrames(t, aliceRec.Body.String()); len(frames) == 0 ||
		frames[len(frames)-1].event != string(types.AnswerEventDone) {
		t.Errorf("alice's frames = %+v, want them through done", frames)
	}
}

func TestAnswerStreamRejectsAnInvalidLastEventID(t *testing.T) {
	svc := &stubAnswer{events: nil, streamDone: true}
	h := newAnswerRouter(svc)
	answerID := uuid.NewString()

	for _, lastEventID := range []string{"-1", "seven", "1.5"} {
		rec := getAnswerEvents(t, h, answerID, lastEventID, "Bearer trelis_alice")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("Last-Event-ID %q = %d %s, want 400", lastEventID, rec.Code, rec.Body)
			continue
		}
		if e := decode(t, rec); e.Success || e.Error == nil || e.Error.Code != 1000 {
			t.Errorf("Last-Event-ID %q body = %s, want the bad-request error envelope", lastEventID, rec.Body)
		}
	}
}

// liveAnswerStream is an AnswerService whose events a test appends while the
// handler is already streaming, so the wait channel proves the wake-up.
type liveAnswerStream struct {
	mu     sync.Mutex
	events []types.AnswerEvent
	wait   chan struct{}
}

func newLiveAnswerStream() *liveAnswerStream {
	return &liveAnswerStream{wait: make(chan struct{})}
}

func (s *liveAnswerStream) Answer(context.Context, *types.AnswerRequest) (*types.AnswerResult, error) {
	return nil, nil
}

func (s *liveAnswerStream) StartAnswerStream(context.Context, *types.AnswerRequest) (uuid.UUID, error) {
	return uuid.Nil, nil
}

func (s *liveAnswerStream) AnswerEvents(_ context.Context, _ uuid.UUID, afterSeq int64) (types.AnswerSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var events []types.AnswerEvent
	for _, event := range s.events {
		if event.Seq > afterSeq {
			events = append(events, event)
		}
	}
	return types.AnswerSubscription{Events: events, Wait: s.wait}, nil
}

func (s *liveAnswerStream) Close(context.Context) error { return nil }

// append stores one Answer Event and wakes the handler waiting for one.
func (s *liveAnswerStream) append(event types.AnswerEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	close(s.wait)
	s.wait = make(chan struct{})
}

func TestAnswerStreamWakesOnAnAppendWithoutPolling(t *testing.T) {
	live := newLiveAnswerStream()
	h := newAnswerRouterWith(keyring{"trelis_alice": alice}, live,
		handler.WithAnswerStreamKeepalive(time.Hour))

	rec := newStreamRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/answers/"+uuid.NewString()+"/events", nil).
		WithContext(ctx)
	req.Header.Set("Authorization", "Bearer trelis_alice")
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, req)
	}()
	select {
	case <-rec.flushes: // the handler wrote its headers and is waiting
	case <-time.After(2 * time.Second):
		t.Fatal("the stream wrote no headers")
	}

	live.append(types.AnswerEvent{Seq: 1, Type: types.AnswerEventToken,
		Data: types.AnswerTokenEvent{Text: "Alpha "}})

	select {
	case <-rec.flushes: // the handler wrote the appended event
	case <-time.After(2 * time.Second):
		t.Fatalf("the handler did not write the appended event; body: %s", rec.bodyString())
	}
	if body := rec.bodyString(); !strings.Contains(body, "event: token") ||
		!strings.Contains(body, `"text":"Alpha "`) {
		t.Errorf("stream body = %s, want the appended token frame", body)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not return after the client left")
	}
}

func TestAnswerStreamWritesKeepalivesWhileIdle(t *testing.T) {
	svc := &stubAnswer{streamID: uuid.New(), streamDone: false}
	h := newAnswerRouterWith(keyring{"trelis_alice": alice}, svc,
		handler.WithAnswerStreamKeepalive(5*time.Millisecond))

	handlerReturned := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		close(handlerReturned)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/answers/"+svc.streamID.String()+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer trelis_alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	sawKeepalive := false
	for scanner.Scan() {
		if scanner.Text() == ": keepalive" {
			sawKeepalive = true
			break
		}
	}
	if !sawKeepalive {
		t.Fatalf("stream wrote no keepalive: %v", scanner.Err())
	}
	_ = resp.Body.Close()
	select {
	case <-handlerReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not return after the client disconnected")
	}
}

// TestShutdownEndsTheAnswerStream drives the real handler through Serve, so it
// fails if streamAnswerEvents stops selecting on the shutdown notice.
func TestShutdownEndsTheAnswerStream(t *testing.T) {
	svc := &stubAnswer{streamID: uuid.New(), streamDone: false}
	h := newAnswerRouterWith(keyring{"trelis_alice": alice}, svc,
		handler.WithAnswerStreamKeepalive(5*time.Millisecond))

	ln := listen(t)
	serveCtx, cancelServe := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(serveCtx, ln, h, 5*time.Second) }()

	reqCtx, cancelReq := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelReq()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"http://"+ln.Addr().String()+"/answers/"+svc.streamID.String()+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer trelis_alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A keepalive frame proves the stream is open before shutdown begins.
	scanner := bufio.NewScanner(resp.Body)
	for {
		if !scanner.Scan() {
			t.Fatalf("stream ended before its first keepalive: %v", scanner.Err())
		}
		if scanner.Text() == ": keepalive" {
			break
		}
	}
	cancelServe() // what SIGTERM does in main

	ended := make(chan error, 1)
	go func() {
		for scanner.Scan() {
		}
		ended <- scanner.Err()
	}()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("reading the stream after shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not end when the server began shutting down")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve = %v, want nil after the stream ended", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the stream ended")
	}
}
