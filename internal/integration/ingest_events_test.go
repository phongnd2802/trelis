//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phongnd2802/trelis/internal/models/modeltest"
	"github.com/phongnd2802/trelis/internal/types"
)

// streamedIngestEvent is one event frame read from an ingest event stream.
type streamedIngestEvent struct {
	Seq      int64
	Type     string
	UploadID string
	Detail   json.RawMessage
}

// ingestEventStream is one open SSE response and its scanner.
type ingestEventStream struct {
	documentID string
	resp       *http.Response
	scanner    *bufio.Scanner
	cancel     context.CancelFunc
	once       sync.Once
}

// openIngestEventStream opens GET /documents/{id}/events as key and checks
// that it is a stream. lastEventID above 0 is sent as the Last-Event-ID
// header.
func openIngestEventStream(t *testing.T, tr *trelis, key, id string, lastEventID int64) *ingestEventStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tr.url+"/documents/"+id+"/events", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if lastEventID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastEventID, 10))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /documents/%s/events: %v", id, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("GET events = %d %s, want 200 with a stream", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	return &ingestEventStream{
		documentID: id,
		resp:       resp,
		scanner:    bufio.NewScanner(resp.Body),
		cancel:     cancel,
	}
}

// close ends the request, so a test can disconnect while the Document's
// ingest is still running. It is safe to call more than once.
func (s *ingestEventStream) close() {
	s.once.Do(func() {
		_ = s.resp.Body.Close()
		s.cancel()
	})
}

// next returns the next event frame, or false at the end of the stream.
func (s *ingestEventStream) next(t *testing.T) (streamedIngestEvent, bool) {
	t.Helper()
	var frame streamedIngestEvent
	var data []byte
	for s.scanner.Scan() {
		line := s.scanner.Text()
		switch {
		case line == "":
			if frame.Type == "" {
				continue
			}
			var payload struct {
				DocumentID string          `json:"document_id"`
				UploadID   string          `json:"upload_id"`
				Type       string          `json:"type"`
				Detail     json.RawMessage `json:"detail"`
			}
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatalf("event %q data %s is not JSON: %v", frame.Type, data, err)
			}
			if payload.DocumentID != s.documentID || payload.Type != frame.Type || payload.UploadID == "" {
				t.Errorf("event %q data = document %q upload %q type %q, want %q, an upload and %q",
					frame.Type, payload.DocumentID, payload.UploadID, payload.Type, s.documentID, frame.Type)
			}
			frame.UploadID = payload.UploadID
			frame.Detail = payload.Detail
			return frame, true
		case strings.HasPrefix(line, ":"):
			// A keepalive comment.
		case strings.HasPrefix(line, "id: "):
			seq, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			if err != nil {
				t.Fatalf("event id in %q is not a seq: %v", line, err)
			}
			frame.Seq = seq
		case strings.HasPrefix(line, "event: "):
			frame.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: ")...)
		default:
			t.Fatalf("unexpected SSE line %q", line)
		}
	}
	if err := s.scanner.Err(); err != nil {
		t.Fatalf("read the event stream of %s: %v", s.documentID, err)
	}
	return streamedIngestEvent{}, false
}

// readIngestEvents opens the stream and reads every frame until it ends.
func readIngestEvents(t *testing.T, tr *trelis, key, id string, lastEventID int64) []streamedIngestEvent {
	t.Helper()
	stream := openIngestEventStream(t, tr, key, id, lastEventID)
	defer stream.close()
	var events []streamedIngestEvent
	for {
		event, ok := stream.next(t)
		if !ok {
			break
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		t.Fatal("the stream ended without an event")
	}
	return events
}

func TestIngestProgressStreamReportsEveryStepThroughCompleted(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	id := tr.upload(t, alice, "notes.md", "# Notes\n\nPhạm vi điều chỉnh\n")

	events := readIngestEvents(t, tr, alice, id, 0)
	if events[0].Type != types.IngestEventParseStarted {
		t.Errorf("first event = %s, want parse_started", events[0].Type)
	}
	if last := events[len(events)-1].Type; last != types.IngestEventCompleted {
		t.Errorf("last event = %s, want completed", last)
	}

	parseFinished, firstEmbed := -1, -1
	for i, event := range events {
		switch event.Type {
		case types.IngestEventParseFinished:
			parseFinished = i
			var detail types.ParseFinishedDetail
			if err := json.Unmarshal(event.Detail, &detail); err != nil || detail.Chunks == 0 {
				t.Errorf("parse_finished detail = %s (%v), want a Chunk count", event.Detail, err)
			}
		case types.IngestEventEmbedBatchFinished:
			if firstEmbed < 0 {
				firstEmbed = i
			}
			var detail types.EmbedBatchFinishedDetail
			if err := json.Unmarshal(event.Detail, &detail); err != nil || detail.Chunks == 0 ||
				detail.From < 0 || detail.To <= detail.From {
				t.Errorf("embed_batch_finished detail = %s (%v), want a stored seq range and count", event.Detail, err)
			}
		}
	}
	if parseFinished < 0 || firstEmbed < 0 {
		t.Fatalf("events = %v, want parse_finished and embed_batch_finished", eventTypes(events))
	}
	if parseFinished <= 0 || parseFinished >= firstEmbed {
		t.Errorf("event order = %v, want parse_started, parse_finished, then an embed batch", eventTypes(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Errorf("event %d seq = %d after %d, want increasing ids", i, events[i].Seq, events[i-1].Seq)
		}
	}

	// Reconnecting with Last-Event-ID replays exactly the events after it.
	resume := events[parseFinished].Seq
	replayed := readIngestEvents(t, tr, alice, id, resume)
	want := make([]streamedIngestEvent, 0, len(events)-parseFinished-1)
	for _, event := range events {
		if event.Seq > resume {
			want = append(want, event)
		}
	}
	if len(replayed) != len(want) {
		t.Fatalf("replayed %d events after seq %d, want the %d later ones: %v vs %v",
			len(replayed), resume, len(want), eventTypes(replayed), eventTypes(want))
	}
	for i := range want {
		if replayed[i].Seq != want[i].Seq || replayed[i].Type != want[i].Type ||
			string(replayed[i].Detail) != string(want[i].Detail) {
			t.Errorf("replayed event %d = %d %s %s, want %d %s %s", i,
				replayed[i].Seq, replayed[i].Type, replayed[i].Detail,
				want[i].Seq, want[i].Type, want[i].Detail)
		}
	}
}

// TestIngestProgressStreamReconnectsMidIngest holds embeddings so the first
// stream can stop at parse_finished, then reconnects with its seq while the
// workflow is still running and reads through to completed.
func TestIngestProgressStreamReconnectsMidIngest(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	tr.provider.Delay(modeltest.Embeddings, 2*time.Second)
	id := tr.upload(t, alice, "notes.md", "# Notes\n\nPhạm vi điều chỉnh\n\nĐiều 1. Nội dung.\n")

	first := openIngestEventStream(t, tr, alice, id, 0)
	defer first.close()
	var events []streamedIngestEvent
	for {
		event, ok := first.next(t)
		if !ok {
			t.Fatalf("the stream ended before parse_finished: %v", eventTypes(events))
		}
		events = append(events, event)
		if event.Type == types.IngestEventParseFinished {
			break
		}
	}
	resume := events[len(events)-1].Seq
	first.close()

	second := openIngestEventStream(t, tr, alice, id, resume)
	defer second.close()
	for {
		event, ok := second.next(t)
		if !ok {
			break
		}
		events = append(events, event)
	}
	if len(events) < 4 {
		t.Fatalf("events = %v, want parse_started, parse_finished, an embed batch and completed", eventTypes(events))
	}
	if last := events[len(events)-1].Type; last != types.IngestEventCompleted {
		t.Errorf("last event = %s, want completed", last)
	}
	for i, event := range events {
		if event.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d, want %d; no gaps or duplicates: %v",
				i, event.Seq, i+1, eventSeqs(events))
		}
	}
}

// eventSeqs renders the seqs of events for a failure message.
func eventSeqs(events []streamedIngestEvent) []int64 {
	seqs := make([]int64, len(events))
	for i, event := range events {
		seqs[i] = event.Seq
	}
	return seqs
}

// eventTypes renders the types of events for a failure message.
func eventTypes(events []streamedIngestEvent) []string {
	names := make([]string, len(events))
	for i, event := range events {
		names[i] = event.Type
	}
	return names
}

func TestIngestProgressStreamIsHiddenFromPrincipalsWhoCannotRead(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	bob := tr.principalKey(t, "bob")
	id := tr.upload(t, alice, "notes.md", "# Notes\n\nChỉ alice đọc được.\n")

	resp := tr.get(t, "/documents/"+id+"/events", bob)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob GET events = %d %s, want 404", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want the JSON error, not an SSE stream", ct)
	}
	missing := tr.get(t, "/documents/0190d2a8-0000-7000-8000-00000000dead/events", bob)
	missingBody, _ := io.ReadAll(missing.Body)
	if string(body) != string(missingBody) {
		t.Errorf("unreadable body %s differs from missing body %s, which reveals existence", body, missingBody)
	}
}

func TestIngestProgressStreamEndsFailedWithTheStoredReason(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	// "not a zip" cannot be a docx in either build: the default build has no
	// engine, and the linked one rejects the bytes.
	id := tr.uploadBytes(t, alice, "broken.docx", []byte("not a zip"))

	events := readIngestEvents(t, tr, alice, id, 0)
	last := events[len(events)-1]
	if last.Type != types.IngestEventFailed {
		t.Fatalf("events = %v, want the stream to end failed", eventTypes(events))
	}
	var detail types.FailedDetail
	if err := json.Unmarshal(last.Detail, &detail); err != nil {
		t.Fatalf("failed detail %s: %v", last.Detail, err)
	}
	doc := tr.waitForIngest(t, alice, id)
	if doc.ParseStatus != "failed" || doc.ParseError == "" {
		t.Fatalf("Document = %q (%q), want failed with its reason", doc.ParseStatus, doc.ParseError)
	}
	if detail.Reason != doc.ParseError {
		t.Errorf("failed event reason = %q, want the Document's stored %q", detail.Reason, doc.ParseError)
	}
	if strings.Contains(detail.Reason, "broken.docx") {
		t.Errorf("failed reason %q names the Source File, want the safe stored reason", detail.Reason)
	}
}
