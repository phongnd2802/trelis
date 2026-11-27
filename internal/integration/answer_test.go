//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

// citationView is one citation of a Pipeline Answer.
type citationView struct {
	Marker     int    `json:"marker"`
	ChunkID    string `json:"chunk_id"`
	DocumentID string `json:"document_id"`
}

// answerDebug is the debug object POST /answer returns when asked for it.
type answerDebug struct {
	Results []retrievedChunk `json:"results"`
	Trace   []traceStep      `json:"trace"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// answerView is the envelope POST /answer answers with.
type answerView struct {
	Success bool `json:"success"`
	Data    struct {
		Answer       string         `json:"answer"`
		Citations    []citationView `json:"citations"`
		FinishReason string         `json:"finish_reason"`
		Debug        *answerDebug   `json:"debug"`
	} `json:"data"`
}

// chunkView is the part of a Chunk GET /chunks/{id} answers with.
type chunkView struct {
	ID            string `json:"id"`
	DocumentID    string `json:"document_id"`
	Seq           int    `json:"seq"`
	Content       string `json:"content"`
	ContextHeader string `json:"context_header"`
	StartOffset   int    `json:"start_offset"`
	EndOffset     int    `json:"end_offset"`
}

// firstChunkID returns the ID of a Document's first Chunk, read straight from
// Postgres, so a test can ask for a Chunk it cannot reach through retrieval.
func (tr *trelis) firstChunkID(t *testing.T, documentID string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, deps.pg.host, deps.pg.port, tr.dbname))
	if err != nil {
		t.Fatalf("connect to database %s: %v", tr.dbname, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var id string
	if err := conn.QueryRow(ctx,
		"SELECT id FROM chunks WHERE document_id = $1 ORDER BY seq LIMIT 1", documentID).Scan(&id); err != nil {
		t.Fatalf("first Chunk of %s: %v", documentID, err)
	}
	return id
}

// TestAnswerCitesOnlyRetrievedChunksAndHonorsDebugAndAccess is the end-to-end
// case for the Pipeline Answer: Bob's answer cites only Chunks retrieval gave
// him, the debug flag returns the results, trace and usage, a bm25 config
// runs BM25 alone, and the chat provider never receives Alice's Restricted
// Document text. The citation's Chunk and Document read back to Bob, while
// Alice's are 404 to him.
func TestAnswerCitesOnlyRetrievedChunksAndHonorsDebugAndAccess(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	bob := tr.principalKey(t, "bob")

	const query = "Thuế TNCN là gì?"
	secretID := tr.upload(t, alice, "secret.md", "# Secret\n\nMã ra mắt là 1234. Thuế TNCN bí mật.\n")
	bobID := tr.upload(t, bob, "bob.md", "# Bob\n\nThuế TNCN là thuế thu nhập cá nhân.\n")
	for _, doc := range []struct{ id, key, name string }{
		{secretID, alice, "Alice's secret"},
		{bobID, bob, "Bob's own"},
	} {
		if got := tr.waitForIngest(t, doc.key, doc.id); got.ParseStatus != "completed" {
			t.Fatalf("%s Document parse_status = %q (%q), want completed", doc.name, got.ParseStatus, got.ParseError)
		}
	}

	// The reply cites the one source and an out-of-range marker the answer
	// must drop.
	tr.provider.Reply(modeltest.Contains(query),
		modeltest.Text("Thuế TNCN là thuế thu nhập cá nhân [1]. Không rõ nguồn [7]."))

	resp := tr.post(t, "/answer", bob, map[string]any{
		"query":            query,
		"top_k":            10,
		"retrieval_config": map[string]any{"techniques": []string{"bm25"}},
		"debug":            true,
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /answer = %d %s, want 200", resp.StatusCode, body)
	}
	var answer answerView
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("decode /answer: %v", err)
	}
	if !answer.Success {
		t.Fatal("POST /answer answered success = false")
	}
	if answer.Data.Debug == nil {
		t.Fatal("debug = nil, want the results, trace and usage")
	}

	// bm25 alone: its search step, then the generate step.
	trace := answer.Data.Debug.Trace
	if len(trace) != 2 {
		t.Fatalf("trace = %+v, want the bm25 search and the generate step", trace)
	}
	if step := trace[0]; step.Step != "search" || step.Technique != "bm25" || step.DurationMS < 0 {
		t.Errorf("trace[0] = %+v, want the bm25 search step", step)
	}
	if step := trace[1]; step.Step != "generate" || step.Technique != "" || step.Model != chatModel || step.DurationMS < 0 {
		t.Errorf("trace[1] = %+v, want the generate step naming %q", step, chatModel)
	}
	usage := answer.Data.Debug.Usage
	if usage.PromptTokens <= 0 || usage.CompletionTokens <= 0 || usage.TotalTokens != usage.PromptTokens+usage.CompletionTokens {
		t.Errorf("usage = %+v, want the chat call's token counts", usage)
	}

	if len(answer.Data.Debug.Results) == 0 {
		t.Fatal("debug results = none, want Bob's readable Chunks")
	}
	retrieved := make(map[string]bool, len(answer.Data.Debug.Results))
	for i, result := range answer.Data.Debug.Results {
		if result.DocumentID != bobID {
			t.Errorf("result %d is from %s, want only Bob's own Document", i, result.DocumentID)
		}
		retrieved[result.ChunkID] = true
	}

	// The only valid citation is source 1, the first retrieved Chunk; the
	// [7] marker is neither cited nor kept in the answer text.
	if len(answer.Data.Citations) != 1 {
		t.Fatalf("citations = %+v, want only the valid source 1", answer.Data.Citations)
	}
	citation := answer.Data.Citations[0]
	if citation.Marker != 1 || !retrieved[citation.ChunkID] || citation.DocumentID != bobID {
		t.Errorf("citation = %+v, want source 1 pointing at a retrieved Chunk of Bob's Document", citation)
	}
	if first := answer.Data.Debug.Results[0]; citation.ChunkID != first.ChunkID {
		t.Errorf("citation chunk = %s, want the first source %s", citation.ChunkID, first.ChunkID)
	}
	if strings.Contains(answer.Data.Answer, "[7]") {
		t.Errorf("answer = %q, want the out-of-range marker removed", answer.Data.Answer)
	}
	if !strings.Contains(answer.Data.Answer, "[1]") {
		t.Errorf("answer = %q, want the valid marker kept", answer.Data.Answer)
	}
	if answer.Data.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", answer.Data.FinishReason)
	}

	// The chat role saw Bob's readable source and the question, never the
	// Restricted Document's text.
	requests := tr.provider.Requests(modeltest.ChatCompletions)
	if len(requests) != 1 {
		t.Fatalf("/chat/completions received %d requests, want exactly the answer call", len(requests))
	}
	var chatBody struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(requests[0].Body, &chatBody); err != nil {
		t.Fatalf("decode chat request: %v", err)
	}
	var prompt strings.Builder
	for _, message := range chatBody.Messages {
		prompt.WriteString(message.Content)
		prompt.WriteString("\n")
	}
	if !strings.Contains(prompt.String(), "Thuế TNCN là thuế thu nhập cá nhân.") {
		t.Errorf("chat prompt %q does not carry Bob's readable source", prompt.String())
	}
	if !strings.Contains(prompt.String(), query) {
		t.Errorf("chat prompt %q does not ask the question", prompt.String())
	}
	if strings.Contains(prompt.String(), "1234") {
		t.Errorf("the chat role received Alice's Restricted Chunk text: %q", prompt.String())
	}

	// The citation opens its Chunk for Bob.
	chunkResp := tr.get(t, "/chunks/"+citation.ChunkID, bob)
	if chunkResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(chunkResp.Body)
		t.Fatalf("GET /chunks/%s = %d %s, want 200", citation.ChunkID, chunkResp.StatusCode, body)
	}
	var chunk struct {
		Data chunkView `json:"data"`
	}
	if err := json.NewDecoder(chunkResp.Body).Decode(&chunk); err != nil {
		t.Fatalf("decode /chunks answer: %v", err)
	}
	if got := chunk.Data; got.ID != citation.ChunkID || got.DocumentID != bobID ||
		!strings.Contains(got.Content, "Thuế TNCN là thuế thu nhập cá nhân.") || got.EndOffset <= got.StartOffset {
		t.Errorf("chunk = %+v, want the cited Chunk of Bob's Document", got)
	}

	// A Chunk of Alice's Restricted Document is 404 to Bob, with the same
	// body as an unknown ID; the Document itself is 404 too. Alice still
	// reads both.
	secretChunk := tr.firstChunkID(t, secretID)
	unreadable := tr.get(t, "/chunks/"+secretChunk, bob)
	unknown := tr.get(t, "/chunks/"+uuid.NewString(), bob)
	unreadableBody, _ := io.ReadAll(unreadable.Body)
	unknownBody, _ := io.ReadAll(unknown.Body)
	if unreadable.StatusCode != http.StatusNotFound || unknown.StatusCode != http.StatusNotFound {
		t.Errorf("GET restricted Chunk = %d, unknown Chunk = %d; want 404 and 404",
			unreadable.StatusCode, unknown.StatusCode)
	}
	if string(unreadableBody) != string(unknownBody) {
		t.Errorf("restricted Chunk body = %s, unknown Chunk body = %s; want the same body",
			unreadableBody, unknownBody)
	}
	if resp := tr.get(t, "/documents/"+secretID, bob); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob GET /documents/%s = %d, want 404", secretID, resp.StatusCode)
	}
	if resp := tr.get(t, "/documents/"+secretID, alice); resp.StatusCode != http.StatusOK {
		t.Errorf("alice GET /documents/%s = %d, want 200", secretID, resp.StatusCode)
	}
	if resp := tr.get(t, "/chunks/"+secretChunk, alice); resp.StatusCode != http.StatusOK {
		t.Errorf("alice GET her own Chunk = %d, want 200", resp.StatusCode)
	}
}
