package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/handler"
	"github.com/phongnd2802/trelis/internal/types"
)

// sseFrame is one parsed Server-Sent Event frame.
type sseFrame struct {
	id    int64
	event string
	data  string
}

// parseSSEFrames splits an SSE body into its event frames, ignoring comments.
func parseSSEFrames(t *testing.T, body string) []sseFrame {
	t.Helper()
	var frames []sseFrame
	var frame sseFrame
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			if frame.event != "" || frame.data != "" {
				frames = append(frames, frame)
			}
			frame = sseFrame{}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("malformed SSE line %q in body:\n%s", line, body)
		}
		switch field {
		case "id":
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Fatalf("SSE id %q is not a seq: %v", value, err)
			}
			frame.id = id
		case "event":
			frame.event = value
		case "data":
			frame.data = value
		default:
			t.Fatalf("unexpected SSE field %q in body:\n%s", field, body)
		}
	}
	return frames
}

// streamEventPayload is the data line's shape; the seq lives only in the id.
type streamEventPayload struct {
	DocumentID uuid.UUID       `json:"document_id"`
	UploadID   uuid.UUID       `json:"upload_id"`
	Type       string          `json:"type"`
	Detail     json.RawMessage `json:"detail"`
	CreatedAt  time.Time       `json:"created_at"`
}

func decodeStreamEvent(t *testing.T, frame sseFrame) streamEventPayload {
	t.Helper()
	var payload streamEventPayload
	if err := json.Unmarshal([]byte(frame.data), &payload); err != nil {
		t.Fatalf("data %q is not an Ingest Event: %v", frame.data, err)
	}
	return payload
}

// seedIngestEvent appends one Ingest Event for the Document's current upload.
func (c *server) seedIngestEvent(t *testing.T, documentID, eventType string, detail any) *types.IngestEvent {
	t.Helper()
	return c.seedIngestEventForUpload(t, documentID, c.documentUploadID(t, documentID), eventType, detail)
}

// seedIngestEventForUpload appends one Ingest Event for the given upload.
func (c *server) seedIngestEventForUpload(t *testing.T, documentID string, uploadID uuid.UUID, eventType string, detail any) *types.IngestEvent {
	t.Helper()
	id, err := uuid.Parse(documentID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	event, err := repository.NewIngestEventRepository(c.store).InsertIngestEvent(c.ctx, &types.IngestEvent{
		DocumentID: id,
		UploadID:   uploadID,
		Type:       eventType,
		Detail:     raw,
	})
	if err != nil {
		t.Fatalf("seed %s event: %v", eventType, err)
	}
	return event
}

// streamIngestEvents GETs the Document's progress stream as the named
// Principal. It returns once the handler has finished.
func (c *server) streamIngestEvents(as, id, lastEventID string) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/documents/"+id+"/events", nil)
	req.Header.Set("Authorization", c.key(as))
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec
}

func TestIngestEventStreamEmitsTheEventsInOrderAndEndsAfterCompleted(t *testing.T) {
	c := newServerWith(t, []handler.DocumentHandlerOption{
		handler.WithIngestEventPollInterval(time.Millisecond),
	}, "alice")
	id := c.uploaded("alice", upFile("luat.txt", "Luật 04/2007"))
	uploadID := c.documentUploadID(t, id)
	seeded := []*types.IngestEvent{
		c.seedIngestEvent(t, id, types.IngestEventParseStarted, types.ParseStartedDetail{Attempt: 1}),
		c.seedIngestEvent(t, id, types.IngestEventParseFinished, types.ParseFinishedDetail{Chunks: 3}),
		c.seedIngestEvent(t, id, types.IngestEventEmbedBatchFinished,
			types.EmbedBatchFinishedDetail{From: 0, To: 3, Chunks: 3}),
		c.seedIngestEvent(t, id, types.IngestEventCompleted, struct{}{}),
	}

	rec := c.streamIngestEvents("alice", id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET events = %d %s, want 200", rec.Code, rec.Body)
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

	frames := parseSSEFrames(t, rec.Body.String())
	if len(frames) != len(seeded) {
		t.Fatalf("streamed %d frames, want %d:\n%s", len(frames), len(seeded), rec.Body)
	}
	for i, frame := range frames {
		if frame.id != seeded[i].Seq {
			t.Errorf("frame %d id = %d, want seq %d", i, frame.id, seeded[i].Seq)
		}
		if frame.event != seeded[i].Type {
			t.Errorf("frame %d event = %q, want %q", i, frame.event, seeded[i].Type)
		}
		data := decodeStreamEvent(t, frame)
		if data.DocumentID.String() != id || data.UploadID != uploadID || data.Type != frame.event || data.CreatedAt.IsZero() {
			t.Errorf("frame %d data = %+v, want the Document, upload, type and created_at", i, data)
		}
	}

	var started types.ParseStartedDetail
	if err := json.Unmarshal(decodeStreamEvent(t, frames[0]).Detail, &started); err != nil || started.Attempt != 1 {
		t.Errorf("parse_started detail = %s (%v), want attempt 1", decodeStreamEvent(t, frames[0]).Detail, err)
	}
	var finished types.ParseFinishedDetail
	if err := json.Unmarshal(decodeStreamEvent(t, frames[1]).Detail, &finished); err != nil || finished.Chunks != 3 {
		t.Errorf("parse_finished detail = %s (%v), want 3 Chunks", decodeStreamEvent(t, frames[1]).Detail, err)
	}
	var embedded types.EmbedBatchFinishedDetail
	if err := json.Unmarshal(decodeStreamEvent(t, frames[2]).Detail, &embedded); err != nil ||
		embedded != (types.EmbedBatchFinishedDetail{From: 0, To: 3, Chunks: 3}) {
		t.Errorf("embed_batch_finished detail = %s (%v), want from 0 to 3 with 3 Chunks",
			decodeStreamEvent(t, frames[2]).Detail, err)
	}
	if got := string(decodeStreamEvent(t, frames[3]).Detail); got != "{}" {
		t.Errorf("completed detail = %s, want {}", got)
	}
}

func TestIngestEventStreamPassesAnOldVersionsTerminalEventAndEndsAtTheCurrentOne(t *testing.T) {
	c := newServerWith(t, []handler.DocumentHandlerOption{
		handler.WithIngestEventPollInterval(time.Millisecond),
	}, "alice")
	id := c.uploaded("alice", upFile("luat.txt", "Luật 04/2007"))
	oldUpload := c.documentUploadID(t, id)
	// The old version's stream, completed before the re-upload.
	c.seedIngestEventForUpload(t, id, oldUpload, types.IngestEventParseStarted,
		types.ParseStartedDetail{Attempt: 1})
	c.seedIngestEventForUpload(t, id, oldUpload, types.IngestEventCompleted, struct{}{})

	newUpload := uuid.New()
	if _, err := c.pool.Exec(c.ctx, "UPDATE documents SET upload_id = $2 WHERE id = $1",
		uuid.MustParse(id), newUpload); err != nil {
		t.Fatalf("re-upload the Document row: %v", err)
	}
	c.seedIngestEventForUpload(t, id, newUpload, types.IngestEventUploadReplaced,
		types.UploadReplacedDetail{UploadID: newUpload})
	c.seedIngestEventForUpload(t, id, newUpload, types.IngestEventCompleted, struct{}{})

	rec := c.streamIngestEvents("alice", id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET events = %d %s, want 200", rec.Code, rec.Body)
	}
	frames := parseSSEFrames(t, rec.Body.String())
	want := []string{
		types.IngestEventParseStarted,
		types.IngestEventCompleted,
		types.IngestEventUploadReplaced,
		types.IngestEventCompleted,
	}
	if len(frames) != len(want) {
		t.Fatalf("streamed %d frames, want %d:\n%s", len(frames), len(want), rec.Body)
	}
	for i, frame := range frames {
		if frame.event != want[i] {
			t.Errorf("frame %d = %q, want %q", i, frame.event, want[i])
		}
	}
	if got := decodeStreamEvent(t, frames[1]).UploadID; got != oldUpload {
		t.Errorf("old completed event upload = %s, want %s", got, oldUpload)
	}
	if got := decodeStreamEvent(t, frames[3]).UploadID; got != newUpload {
		t.Errorf("current completed event upload = %s, want %s", got, newUpload)
	}
}

func TestIngestEventStreamReplaysOnlyEventsAfterLastEventID(t *testing.T) {
	c := newServerWith(t, []handler.DocumentHandlerOption{
		handler.WithIngestEventPollInterval(time.Millisecond),
	}, "alice")
	id := c.uploaded("alice", upFile("luat.txt", "Luật 04/2007"))
	seeded := []*types.IngestEvent{
		c.seedIngestEvent(t, id, types.IngestEventParseStarted, types.ParseStartedDetail{Attempt: 1}),
		c.seedIngestEvent(t, id, types.IngestEventParseFinished, types.ParseFinishedDetail{Chunks: 3}),
		c.seedIngestEvent(t, id, types.IngestEventEmbedBatchFinished,
			types.EmbedBatchFinishedDetail{From: 0, To: 3, Chunks: 3}),
		c.seedIngestEvent(t, id, types.IngestEventCompleted, struct{}{}),
	}
	resumeAfter := seeded[1].Seq

	rec := c.streamIngestEvents("alice", id, strconv.FormatInt(resumeAfter, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET events with Last-Event-ID = %d %s, want 200", rec.Code, rec.Body)
	}
	frames := parseSSEFrames(t, rec.Body.String())
	want := seeded[2:]
	if len(frames) != len(want) {
		t.Fatalf("streamed %d frames after seq %d, want the %d later events:\n%s",
			len(frames), resumeAfter, len(want), rec.Body)
	}
	for i, frame := range frames {
		if frame.id != want[i].Seq || frame.event != want[i].Type {
			t.Errorf("frame %d = %d %s, want %d %s", i, frame.id, frame.event, want[i].Seq, want[i].Type)
		}
	}
	for _, frame := range frames {
		if frame.id <= resumeAfter {
			t.Errorf("frame id %d replayed an event at or before Last-Event-ID %d", frame.id, resumeAfter)
		}
	}
}

func TestAnUnreadableDocumentGetsTheSameNotFoundAsAMissingOne(t *testing.T) {
	c := newServer(t, "alice", "bob")
	id := c.uploaded("alice", upFile("luong.txt", "Bảng lương 2024"))

	unreadable := c.streamIngestEvents("bob", id, "")
	missing := c.streamIngestEvents("bob", "0190d2a8-0000-7000-8000-00000000dead", "")
	if unreadable.Code != http.StatusNotFound {
		t.Fatalf("bob GET events = %d %s, want 404", unreadable.Code, unreadable.Body)
	}
	if unreadable.Body.String() != missing.Body.String() {
		t.Errorf("unreadable body %s differs from missing body %s, which reveals existence",
			unreadable.Body, missing.Body)
	}
	if ct := unreadable.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want the JSON error, not an SSE stream", ct)
	}
	if unreadable.Header().Get("X-Accel-Buffering") != "" {
		t.Error("the 404 carries SSE headers, want none")
	}
}

func TestIngestEventStreamRejectsInvalidInput(t *testing.T) {
	c := newServer(t, "alice")
	id := c.uploaded("alice", upFile("luat.txt", "Luật 04/2007"))

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"invalid id":                c.streamIngestEvents("alice", "not-a-uuid", ""),
		"negative Last-Event-ID":    c.streamIngestEvents("alice", id, "-1"),
		"non-numeric Last-Event-ID": c.streamIngestEvents("alice", id, "seven"),
		"fractional Last-Event-ID":  c.streamIngestEvents("alice", id, "1.5"),
	} {
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, rec.Code, rec.Body)
			continue
		}
		if e := decode(t, rec); e.Success || e.Error == nil || e.Error.Code != 1000 {
			t.Errorf("%s body = %s, want the bad-request error envelope", name, rec.Body)
		}
	}
}

// streamRecorder is a ResponseWriter whose flushed frames a test can wait for
// while the handler is still polling.
type streamRecorder struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    bytes.Buffer
	flushes chan struct{}
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: http.Header{}, flushes: make(chan struct{}, 16)}
}

func (r *streamRecorder) Header() http.Header { return r.header }

func (r *streamRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		r.status = status
	}
}

func (r *streamRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(p)
}

func (r *streamRecorder) Flush() {
	select {
	case r.flushes <- struct{}{}:
	default:
	}
}

func (r *streamRecorder) bodyString() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

// openStream starts the progress stream in a goroutine and waits for its
// first flushed frame.
func (c *server) openStream(t *testing.T, id string) (*streamRecorder, chan struct{}) {
	t.Helper()
	rec := newStreamRecorder()
	req := httptest.NewRequest(http.MethodGet, "/documents/"+id+"/events", nil)
	req.Header.Set("Authorization", c.key("alice"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.h.ServeHTTP(rec, req)
	}()
	select {
	case <-rec.flushes:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream wrote no first frame")
	}
	return rec, done
}

// waitForStreamEnd waits for the handler to return after it lost the right to
// read the Document.
func waitForStreamEnd(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not end after the Document became unreadable")
	}
}

func (c *server) principalID(t *testing.T, name string) uuid.UUID {
	t.Helper()
	p, err := repository.NewPrincipalRepository(c.store).GetPrincipalByName(c.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func TestRevokingAccessEndsTheStream(t *testing.T) {
	c := newServerWith(t, []handler.DocumentHandlerOption{
		handler.WithIngestEventPollInterval(time.Millisecond),
	}, "alice")
	id := c.uploaded("alice", upFile("luat.txt", "Luật 04/2007"))
	c.seedIngestEvent(t, id, types.IngestEventParseStarted, types.ParseStartedDetail{Attempt: 1})
	rec, done := c.openStream(t, id)

	if _, err := c.pool.Exec(c.ctx, "DELETE FROM document_principals WHERE document_id = $1 AND principal_id = $2",
		uuid.MustParse(id), c.principalID(t, "alice")); err != nil {
		t.Fatalf("revoke the grant: %v", err)
	}
	waitForStreamEnd(t, done)

	if frames := parseSSEFrames(t, rec.bodyString()); len(frames) != 1 {
		t.Errorf("stream wrote %d frames, want only the one before the revoke:\n%s", len(frames), rec.bodyString())
	}
}

func TestDeletingTheDocumentEndsTheStream(t *testing.T) {
	c := newServerWith(t, []handler.DocumentHandlerOption{
		handler.WithIngestEventPollInterval(time.Millisecond),
	}, "alice")
	id := c.uploaded("alice", upFile("luat.txt", "Luật 04/2007"))
	c.seedIngestEvent(t, id, types.IngestEventParseStarted, types.ParseStartedDetail{Attempt: 1})
	rec, done := c.openStream(t, id)

	if _, err := c.pool.Exec(c.ctx, "DELETE FROM documents WHERE id = $1", uuid.MustParse(id)); err != nil {
		t.Fatalf("delete the Document: %v", err)
	}
	waitForStreamEnd(t, done)

	if frames := parseSSEFrames(t, rec.bodyString()); len(frames) != 1 {
		t.Errorf("stream wrote %d frames, want only the one before the delete:\n%s", len(frames), rec.bodyString())
	}
}

func TestAClientDisconnectEndsTheStream(t *testing.T) {
	c := newServerWith(t, []handler.DocumentHandlerOption{
		handler.WithIngestEventPollInterval(time.Millisecond),
	}, "alice")
	id := c.uploaded("alice", upFile("luat.txt", "Luật 04/2007"))
	c.seedIngestEvent(t, id, types.IngestEventParseStarted, types.ParseStartedDetail{Attempt: 1})

	handlerReturned := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.h.ServeHTTP(w, r)
		close(handlerReturned)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/documents/"+id+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", c.key("alice"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read the first frame: %v", err)
		}
		if line == "\n" {
			break
		}
	}
	cancel()
	_ = resp.Body.Close()

	select {
	case <-handlerReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not return after the client disconnected")
	}
}

func TestIngestEventStreamWritesKeepalivesWhileIdle(t *testing.T) {
	c := newServerWith(t, []handler.DocumentHandlerOption{
		handler.WithIngestEventPollInterval(2 * time.Millisecond),
		handler.WithIngestEventKeepalive(5 * time.Millisecond),
	}, "alice")
	id := c.uploaded("alice", upFile("luat.txt", "Luật 04/2007"))
	c.seedIngestEvent(t, id, types.IngestEventParseStarted, types.ParseStartedDetail{Attempt: 1})

	handlerReturned := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.h.ServeHTTP(w, r)
		close(handlerReturned)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/documents/"+id+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", c.key("alice"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	sawEvent, sawKeepalive := false, false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "event: "+types.IngestEventParseStarted {
			sawEvent = true
		}
		if line == ": keepalive" {
			sawKeepalive = true
			break
		}
	}
	if !sawEvent || !sawKeepalive {
		t.Fatalf("stream saw the event %v and a keepalive %v, want both: %v", sawEvent, sawKeepalive, scanner.Err())
	}
	_ = resp.Body.Close()
	select {
	case <-handlerReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not return after the client disconnected")
	}
}

func TestIngestEventStreamEndsAfterItsCap(t *testing.T) {
	c := newServerWith(t, []handler.DocumentHandlerOption{
		handler.WithIngestEventPollInterval(2 * time.Millisecond),
		handler.WithIngestEventTimeout(20 * time.Millisecond),
	}, "alice")
	id := c.uploaded("alice", upFile("luat.txt", "Luật 04/2007"))
	c.seedIngestEvent(t, id, types.IngestEventParseStarted, types.ParseStartedDetail{Attempt: 1})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/documents/"+id+"/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", c.key("alice"))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		c.h.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("the stream did not end after its 20ms cap")
	}
	frames := parseSSEFrames(t, rec.Body.String())
	if len(frames) != 1 || frames[0].event != types.IngestEventParseStarted {
		t.Errorf("frames = %+v, want only the parse_started written before the cap", frames)
	}
	if strings.Contains(rec.Body.String(), "keepalive") {
		t.Errorf("body has a keepalive before the 20ms cap:\n%s", rec.Body)
	}
}

// TestShutdownEndsTheIngestEventStream drives the real handler through Serve,
// so it fails if StreamIngestEvents stops selecting on the shutdown notice.
func TestShutdownEndsTheIngestEventStream(t *testing.T) {
	c := newServerWith(t, []handler.DocumentHandlerOption{
		handler.WithIngestEventPollInterval(time.Millisecond),
	}, "alice")
	id := c.uploaded("alice", upFile("luat.txt", "Luật 04/2007"))
	c.seedIngestEvent(t, id, types.IngestEventParseStarted, types.ParseStartedDetail{Attempt: 1})

	ln := listen(t)
	serveCtx, cancelServe := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(serveCtx, ln, c.h, 5*time.Second) }()

	reqCtx, cancelReq := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelReq()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"http://"+ln.Addr().String()+"/documents/"+id+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", c.key("alice"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	for {
		if !scanner.Scan() {
			t.Fatalf("stream ended before its first frame: %v", scanner.Err())
		}
		if scanner.Text() == "" {
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
