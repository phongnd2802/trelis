//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

// answerStreamFrame is one parsed Server-Sent Event frame of a streamed
// answer.
type answerStreamFrame struct {
	ID    int64
	Event string
	Data  json.RawMessage
}

// readAnswerStreamFrame reads one frame; io.EOF means the stream ended
// cleanly between frames.
func readAnswerStreamFrame(r *bufio.Reader) (answerStreamFrame, error) {
	var frame answerStreamFrame
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && frame.Event == "" {
				return frame, io.EOF
			}
			return frame, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if frame.Event != "" {
				return frame, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch field {
		case "id":
			if _, err := fmt.Sscan(value, &frame.ID); err != nil {
				return frame, fmt.Errorf("SSE id %q: %w", value, err)
			}
		case "event":
			frame.Event = value
		case "data":
			frame.Data = json.RawMessage(value)
		}
	}
}

// collectAnswerStream reads frames until the stream ends.
func collectAnswerStream(t *testing.T, body io.Reader) []answerStreamFrame {
	t.Helper()
	reader := bufio.NewReader(body)
	var frames []answerStreamFrame
	for {
		frame, err := readAnswerStreamFrame(reader)
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatalf("read answer stream: %v", err)
		}
		frames = append(frames, frame)
	}
}

// streamAnswer sends POST /answer with Accept: text/event-stream. The caller
// closes the body.
func (tr *trelis) streamAnswer(t *testing.T, ctx context.Context, key string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tr.url+"/answer", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /answer as a stream: %v", err)
	}
	return resp
}

// answerEvents sends GET /answers/{id}/events, with Last-Event-ID when
// lastEventID is not empty. The caller closes the body.
func (tr *trelis) answerEvents(t *testing.T, ctx context.Context, key, answerID, lastEventID string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		tr.url+"/answers/"+answerID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /answers/%s/events: %v", answerID, err)
	}
	return resp
}

// streamTokenPayload and the others are the data bodies of the answer events.
type streamTokenPayload struct {
	Text string `json:"text"`
}

type streamCitationPayload struct {
	Marker     int    `json:"marker"`
	ChunkID    string `json:"chunk_id"`
	DocumentID string `json:"document_id"`
}

type streamDonePayload struct {
	Answer       string       `json:"answer"`
	FinishReason string       `json:"finish_reason"`
	Debug        *answerDebug `json:"debug"`
}

// decodeFrame decodes a frame's data as T.
func decodeFrame[T any](t *testing.T, frame answerStreamFrame) T {
	t.Helper()
	var payload T
	if err := json.Unmarshal(frame.Data, &payload); err != nil {
		t.Fatalf("data %q of event %q: %v", frame.Data, frame.Event, err)
	}
	return payload
}

// TestStreamedAnswerEmitsTokensAndCitationsAndResumes is the end-to-end case
// for streamed Pipeline Answers: tokens arrive incrementally, the citations
// point at retrieved Chunks of the uploaded Document, done carries the same
// answer the JSON route returns, dropping the connection after the first
// token still lets a Last-Event-ID resume deliver exactly the rest, and
// another Principal cannot resume it.
func TestStreamedAnswerEmitsTokensAndCitationsAndResumes(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	bob := tr.principalKey(t, "bob")

	documentID := tr.upload(t, alice, "thue.md",
		"# Thuế\n\nThuế TNCN là thuế thu nhập cá nhân. Mức giảm trừ gia cảnh là mười một triệu đồng mỗi tháng.\n")
	if got := tr.waitForIngest(t, alice, documentID); got.ParseStatus != "completed" {
		t.Fatalf("Document parse_status = %q (%q), want completed", got.ParseStatus, got.ParseError)
	}

	const query = "Thuế TNCN là gì?"
	const reply = "Thuế TNCN là thuế thu nhập cá nhân [1]. Mức giảm trừ gia cảnh là mười một triệu đồng mỗi tháng [1]."
	tr.provider.Reply(modeltest.Contains(query), modeltest.Text(reply))
	request := map[string]any{
		"query":            query,
		"top_k":            10,
		"retrieval_config": map[string]any{"techniques": []string{"bm25"}},
		"debug":            true,
	}

	resp := tr.streamAnswer(t, context.Background(), alice, request)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /answer with Accept: text/event-stream = %d %s, want 200", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	full := collectAnswerStream(t, resp.Body)
	if len(full) == 0 {
		t.Fatal("the stream wrote no frames")
	}

	var (
		tokens    []string
		citations []streamCitationPayload
		done      *streamDonePayload
	)
	for i, frame := range full {
		if frame.ID != int64(i+1) {
			t.Errorf("frame %d id = %d, want seq %d", i, frame.ID, i+1)
		}
		switch frame.Event {
		case "started":
			started := decodeFrame[struct {
				AnswerID string `json:"answer_id"`
			}](t, frame)
			if started.AnswerID == "" {
				t.Error("started carries no answer_id")
			}
		case "token":
			tokens = append(tokens, decodeFrame[streamTokenPayload](t, frame).Text)
		case "citation":
			citations = append(citations, decodeFrame[streamCitationPayload](t, frame))
		case "done":
			payload := decodeFrame[streamDonePayload](t, frame)
			done = &payload
		default:
			t.Errorf("frame %d has unexpected event %q", i, frame.Event)
		}
	}
	if full[0].Event != "started" {
		t.Errorf("first event = %q, want started", full[0].Event)
	}
	if full[len(full)-1].Event != "done" {
		t.Errorf("last event = %q, want done", full[len(full)-1].Event)
	}
	if len(tokens) < 2 {
		t.Fatalf("streamed %d token events, want at least two before done", len(tokens))
	}
	if got := strings.Join(tokens, ""); got != reply {
		t.Errorf("tokens concatenate to %q, want the raw reply %q", got, reply)
	}
	if done == nil {
		t.Fatal("no done event")
	}
	if len(citations) == 0 {
		t.Fatal("no citation event, want the cited source")
	}
	if done.Debug == nil || len(done.Debug.Results) == 0 {
		t.Fatal("done debug = nil, want the retrieved Chunks")
	}
	retrieved := make(map[string]bool, len(done.Debug.Results))
	for _, result := range done.Debug.Results {
		retrieved[result.ChunkID] = true
	}
	for _, citation := range citations {
		if citation.Marker != 1 {
			t.Errorf("citation marker = %d, want source 1", citation.Marker)
		}
		if !retrieved[citation.ChunkID] {
			t.Errorf("citation chunk %s is not one of the retrieved Chunks", citation.ChunkID)
		}
		if citation.DocumentID != documentID {
			t.Errorf("citation document = %s, want the uploaded Document %s", citation.DocumentID, documentID)
		}
	}
	if !strings.Contains(done.Answer, "[1]") {
		t.Errorf("done answer = %q, want the valid marker kept", done.Answer)
	}
	if done.FinishReason != "stop" {
		t.Errorf("done finish_reason = %q, want stop", done.FinishReason)
	}

	// The JSON route returns the same answer for the same scripted reply.
	jsonResp := tr.post(t, "/answer", alice, request)
	if jsonResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(jsonResp.Body)
		t.Fatalf("POST /answer as JSON = %d %s, want 200", jsonResp.StatusCode, body)
	}
	var jsonAnswer answerView
	if err := json.NewDecoder(jsonResp.Body).Decode(&jsonAnswer); err != nil {
		t.Fatalf("decode /answer: %v", err)
	}
	if done.Answer != jsonAnswer.Data.Answer {
		t.Errorf("done answer = %q, want the JSON answer %q", done.Answer, jsonAnswer.Data.Answer)
	}
	if len(citations) != len(jsonAnswer.Data.Citations) {
		t.Fatalf("streamed %d citations, JSON answered %d", len(citations), len(jsonAnswer.Data.Citations))
	}
	for i, citation := range citations {
		want := jsonAnswer.Data.Citations[i]
		if citation.Marker != want.Marker || citation.ChunkID != want.ChunkID ||
			citation.DocumentID != want.DocumentID {
			t.Errorf("citation %d = %+v, want %+v from the JSON answer", i, citation, want)
		}
	}

	// Drop the connection after the first token, then resume from it while the
	// provider still holds the stream, so the resume is demonstrably live.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseStream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseStream)
	tr.provider.StreamHold(release)
	t.Cleanup(func() { tr.provider.StreamHold(nil) })

	dropCtx, drop := context.WithCancel(context.Background())
	dropped := tr.streamAnswer(t, dropCtx, alice, request)
	reader := bufio.NewReader(dropped.Body)
	var answerID string
	var firstToken answerStreamFrame
	for firstToken.Event != "token" {
		frame, err := readAnswerStreamFrame(reader)
		if errors.Is(err, io.EOF) {
			t.Fatalf("stream ended before its first token; frames: %+v", full)
		}
		if err != nil {
			t.Fatalf("read the dropped stream: %v", err)
		}
		if frame.Event == "started" {
			answerID = decodeFrame[struct {
				AnswerID string `json:"answer_id"`
			}](t, frame).AnswerID
		}
		if frame.Event == "token" {
			firstToken = frame
		}
	}
	// The provider holds after the first text chunk, so this token is read
	// before the stream sends its last chunk.
	select {
	case <-release:
		t.Fatal("the provider released the stream before the first token was read")
	default:
	}
	if chunks := tr.provider.StreamedTextChunks(); chunks != 1 {
		t.Fatalf("the provider sent %d text chunks before the first token was read, want it held after 1", chunks)
	}
	drop()
	_ = dropped.Body.Close()
	if answerID == "" {
		t.Fatal("the dropped stream never named its answer_id")
	}

	resumeCtx, cancelResume := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelResume()
	resumedResp := tr.answerEvents(t, resumeCtx, alice, answerID, fmt.Sprint(firstToken.ID))
	defer func() { _ = resumedResp.Body.Close() }()
	if resumedResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resumedResp.Body)
		t.Fatalf("GET resumed events = %d %s, want 200", resumedResp.StatusCode, body)
	}
	releaseStream()
	resumed := collectAnswerStream(t, resumedResp.Body)

	// Everything after the token the dropped client saw, in the same order
	// and with the same payloads, through done.
	var tail []answerStreamFrame
	for _, frame := range full {
		if frame.ID > firstToken.ID {
			tail = append(tail, frame)
		}
	}
	if len(resumed) != len(tail) {
		t.Fatalf("resume wrote %d frames, want the %d after seq %d", len(resumed), len(tail), firstToken.ID)
	}
	for i, frame := range resumed {
		want := tail[i]
		if frame.ID != want.ID || frame.Event != want.Event {
			t.Errorf("resumed frame %d = %d %s, want %d %s", i, frame.ID, frame.Event, want.ID, want.Event)
			continue
		}
		switch frame.Event {
		case "token":
			if got, wantText := decodeFrame[streamTokenPayload](t, frame), decodeFrame[streamTokenPayload](t, want); got != wantText {
				t.Errorf("resumed token %d = %q, want %q", i, got.Text, wantText.Text)
			}
		case "citation":
			if got, wantCitation := decodeFrame[streamCitationPayload](t, frame), decodeFrame[streamCitationPayload](t, want); got != wantCitation {
				t.Errorf("resumed citation %d = %+v, want %+v", i, got, wantCitation)
			}
		case "done":
			got, wantDone := decodeFrame[streamDonePayload](t, frame), decodeFrame[streamDonePayload](t, want)
			if got.Answer != wantDone.Answer || got.FinishReason != wantDone.FinishReason {
				t.Errorf("resumed done = %+v, want %+v", got, wantDone)
			}
		}
	}
	if resumed[len(resumed)-1].Event != "done" {
		t.Errorf("last resumed event = %q, want done", resumed[len(resumed)-1].Event)
	}

	// Another Principal cannot resume the answer; the body is the same as for
	// an unknown ID.
	unreadable := tr.answerEvents(t, context.Background(), bob, answerID, "")
	unknown := tr.answerEvents(t, context.Background(), bob, uuid.NewString(), "")
	defer func() { _ = unreadable.Body.Close() }()
	defer func() { _ = unknown.Body.Close() }()
	unreadableBody, _ := io.ReadAll(unreadable.Body)
	unknownBody, _ := io.ReadAll(unknown.Body)
	if unreadable.StatusCode != http.StatusNotFound || unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("bob resume = %d and unknown = %d, want 404 and 404", unreadable.StatusCode, unknown.StatusCode)
	}
	if string(unreadableBody) != string(unknownBody) {
		t.Errorf("another Principal's body %s differs from an unknown ID's %s, which reveals existence",
			unreadableBody, unknownBody)
	}
	if ct := unreadable.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want the JSON error, not an SSE stream", ct)
	}
}
