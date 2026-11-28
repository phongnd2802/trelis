//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

// expectedNoSourcesVietnamese is the fixed no-source answer for the
// Vietnamese follow-up used below.
const expectedNoSourcesVietnamese = "Tôi không tìm thấy thông tin nào bạn được phép đọc để trả lời câu hỏi này."

// conversationTurnView is one turn GET /conversations/{id} answers with.
type conversationTurnView struct {
	Seq            int64     `json:"seq"`
	Question       string    `json:"question"`
	RewrittenQuery *string   `json:"rewritten_query"`
	Answer         string    `json:"answer"`
	Withheld       bool      `json:"withheld"`
	CreatedAt      time.Time `json:"created_at"`
}

// conversationView is the envelope of a Conversation route.
type conversationView struct {
	Success bool `json:"success"`
	Data    struct {
		ID        string                 `json:"id"`
		CreatedAt time.Time              `json:"created_at"`
		Turns     []conversationTurnView `json:"turns"`
	} `json:"data"`
}

// createConversation POSTs an empty object to /conversations and returns the
// new Conversation's ID.
func (tr *trelis) createConversation(t *testing.T, key string) string {
	t.Helper()
	resp := tr.post(t, "/conversations", key, map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /conversations = %d %s, want 201", resp.StatusCode, body)
	}
	var created conversationView
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode POST /conversations: %v", err)
	}
	if created.Data.ID == "" || created.Data.CreatedAt.IsZero() {
		t.Fatalf("created Conversation = %+v, want its id and created_at", created.Data)
	}
	return created.Data.ID
}

// askInConversation POSTs /answer for conversationID and returns the decoded
// answer, failing unless it succeeded.
func (tr *trelis) askInConversation(t *testing.T, key, conversationID string, body map[string]any) answerView {
	t.Helper()
	body["conversation_id"] = conversationID
	body["debug"] = true
	resp := tr.post(t, "/answer", key, body)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /answer = %d %s, want 200", resp.StatusCode, raw)
	}
	var answer answerView
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("decode POST /answer: %v", err)
	}
	if answer.Data.Debug == nil {
		t.Fatal("debug = nil, want the results, trace and usage")
	}
	return answer
}

// chatPromptText joins the request's message texts, the way the fake
// provider's rules see them.
func chatPromptText(t *testing.T, body []byte) string {
	t.Helper()
	var chatBody struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &chatBody); err != nil {
		t.Fatalf("decode chat request: %v", err)
	}
	var prompt strings.Builder
	for _, message := range chatBody.Messages {
		prompt.WriteString(message.Content)
		prompt.WriteString("\n")
	}
	return prompt.String()
}

// embeddingInputs returns the texts an embeddings request asked to embed.
func embeddingInputs(t *testing.T, body []byte) []string {
	t.Helper()
	var req struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode embeddings request: %v", err)
	}
	var one string
	if err := json.Unmarshal(req.Input, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(req.Input, &many); err != nil {
		t.Fatalf("embeddings input %s is neither a string nor a string list: %v", req.Input, err)
	}
	return many
}

// findRewriteStep returns the trace's rewrite_query step, or nil when none
// ran.
func findRewriteStep(trace []traceStep) *traceStep {
	for i := range trace {
		if trace[i].Step == "rewrite_query" {
			return &trace[i]
		}
	}
	return nil
}

func TestConversationHistoryRewritesAFollowUpAndStaysPrivate(t *testing.T) {
	tr := start(t)
	alice, bob := tr.principalKey(t, "alice"), tr.principalKey(t, "bob")

	docID := tr.upload(t, alice, "tax.md",
		"# Thuế\n\nThuế TNCN là thuế thu nhập cá nhân.\n\nThuế TNCN năm 2024 là 10%.\n")
	if got := tr.waitForIngest(t, alice, docID); got.ParseStatus != "completed" {
		t.Fatalf("Document parse_status = %q (%q), want completed", got.ParseStatus, got.ParseError)
	}

	// The rewrite prompt asks for the standalone query; the answer prompt
	// carries the sources and the question. "Question:" with a capital Q
	// matches only the answer call.
	tr.provider.Reply(modeltest.LastContains("Latest question:"), modeltest.Text("Thuế TNCN năm 2024"))
	tr.provider.Reply(modeltest.Contains("Question:"), modeltest.Text("Thuế TNCN là thuế thu nhập cá nhân [1]."))

	conversationID := tr.createConversation(t, alice)
	techniques := map[string]any{"techniques": []string{"bm25", "query_rewrite"}}

	first := tr.askInConversation(t, alice, conversationID, map[string]any{
		"query":            "Thuế TNCN là gì?",
		"retrieval_config": techniques,
	})
	if step := findRewriteStep(first.Data.Debug.Trace); step != nil {
		t.Errorf("the first question carried history and rewrote: %+v", step)
	}

	second := tr.askInConversation(t, alice, conversationID, map[string]any{
		"query":            "và năm 2024 thì sao?",
		"retrieval_config": techniques,
	})
	trace := second.Data.Debug.Trace
	if len(trace) != 3 {
		t.Fatalf("trace = %+v, want rewrite_query, the bm25 search and generate", trace)
	}
	rewrite := trace[0]
	if rewrite.Step != "rewrite_query" || rewrite.Technique != "query_rewrite" ||
		rewrite.Model != chatModel || rewrite.RewrittenQuery != "Thuế TNCN năm 2024" ||
		rewrite.Usage == nil || rewrite.Usage.TotalTokens <= 0 {
		t.Errorf("trace[0] = %+v, want the rewrite_query step with the rewritten text and its usage", rewrite)
	}
	if trace[1].Step != "search" || trace[1].Technique != "bm25" {
		t.Errorf("trace[1] = %+v, want the bm25 search step", trace[1])
	}
	if trace[2].Step != "generate" || trace[2].Model != chatModel {
		t.Errorf("trace[2] = %+v, want the generate step", trace[2])
	}
	usage := second.Data.Debug.Usage
	if usage.TotalTokens <= rewrite.Usage.TotalTokens {
		t.Errorf("debug usage = %+v, want the generate call's tokens on top of the rewrite's %+v", usage, *rewrite.Usage)
	}

	// The answer generation asked the rewritten query, never the raw
	// follow-up.
	requests := tr.provider.Requests(modeltest.ChatCompletions)
	if len(requests) != 3 {
		t.Fatalf("/chat/completions received %d requests, want the first answer, the rewrite and the second answer", len(requests))
	}
	answerPrompt := chatPromptText(t, requests[2].Body)
	if !strings.Contains(answerPrompt, "Question: Thuế TNCN năm 2024") {
		t.Errorf("second answer prompt %q does not ask the rewritten query", answerPrompt)
	}
	if strings.Contains(answerPrompt, "Question: và năm 2024 thì sao?") {
		t.Errorf("second answer prompt %q asks the raw follow-up", answerPrompt)
	}
	if rewritePrompt := chatPromptText(t, requests[1].Body); !strings.Contains(rewritePrompt, "Latest question: và năm 2024 thì sao?") {
		t.Errorf("rewrite prompt %q does not carry the latest question", rewritePrompt)
	}

	// The Conversation carries both turns, the second with its rewritten
	// query.
	resp := tr.get(t, "/conversations/"+conversationID, alice)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /conversations/%s = %d %s, want 200", conversationID, resp.StatusCode, body)
	}
	var conversation conversationView
	if err := json.NewDecoder(resp.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode GET /conversations: %v", err)
	}
	if len(conversation.Data.Turns) != 2 {
		t.Fatalf("turns = %+v, want the two answered questions", conversation.Data.Turns)
	}
	if turn := conversation.Data.Turns[0]; turn.Seq != 1 || turn.Question != "Thuế TNCN là gì?" ||
		turn.RewrittenQuery != nil {
		t.Errorf("turn 1 = %+v, want the raw first question and no rewritten query", turn)
	}
	if turn := conversation.Data.Turns[1]; turn.Seq != 2 || turn.Question != "và năm 2024 thì sao?" ||
		turn.RewrittenQuery == nil || *turn.RewrittenQuery != "Thuế TNCN năm 2024" {
		t.Errorf("turn 2 = %+v, want the raw follow-up with its rewritten query", turn)
	}

	// Another Principal gets the same 404 as an unknown id on both routes,
	// and causes no model call.
	before := len(tr.provider.Requests(modeltest.ChatCompletions))
	foreign := tr.get(t, "/conversations/"+conversationID, bob)
	unknown := tr.get(t, "/conversations/"+uuid.NewString(), bob)
	malformed := tr.get(t, "/conversations/not-a-uuid", bob)
	foreignBody, _ := io.ReadAll(foreign.Body)
	unknownBody, _ := io.ReadAll(unknown.Body)
	malformedBody, _ := io.ReadAll(malformed.Body)
	if foreign.StatusCode != http.StatusNotFound || unknown.StatusCode != http.StatusNotFound ||
		malformed.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign = %d, unknown = %d, malformed = %d; want 404 everywhere",
			foreign.StatusCode, unknown.StatusCode, malformed.StatusCode)
	}
	if string(foreignBody) != string(unknownBody) || string(foreignBody) != string(malformedBody) {
		t.Errorf("bodies differ:\nforeign:   %s\nunknown:   %s\nmalformed: %s",
			foreignBody, unknownBody, malformedBody)
	}

	foreignAnswer := tr.post(t, "/answer", bob, map[string]any{
		"query":            "who won?",
		"conversation_id":  conversationID,
		"retrieval_config": techniques,
	})
	foreignAnswerBody, _ := io.ReadAll(foreignAnswer.Body)
	if foreignAnswer.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /answer for another Principal = %d %s, want 404", foreignAnswer.StatusCode, foreignAnswerBody)
	}
	if string(foreignAnswerBody) != string(unknownBody) {
		t.Errorf("POST /answer 404 body = %s, want the same as an unknown Conversation %s",
			foreignAnswerBody, unknownBody)
	}
	if after := len(tr.provider.Requests(modeltest.ChatCompletions)); after != before {
		t.Errorf("a rejected foreign Conversation made %d model calls, want none", after-before)
	}

	// With query_rewrite off, a follow-up in a Conversation searches its raw
	// question and records no rewritten query. Dense runs so the embedding
	// request shows the query actually searched.
	plainID := tr.createConversation(t, alice)
	plain := map[string]any{"techniques": []string{"dense"}}
	tr.askInConversation(t, alice, plainID, map[string]any{
		"query":            "Thuế TNCN là gì?",
		"retrieval_config": plain,
	})
	off := tr.askInConversation(t, alice, plainID, map[string]any{
		"query":            "và năm 2024 thì sao?",
		"retrieval_config": plain,
	})
	if step := findRewriteStep(off.Data.Debug.Trace); step != nil {
		t.Errorf("with query_rewrite off the trace still has a rewrite step: %+v", step)
	}
	requests = tr.provider.Requests(modeltest.ChatCompletions)
	lastPrompt := chatPromptText(t, requests[len(requests)-1].Body)
	if !strings.Contains(lastPrompt, "Question: và năm 2024 thì sao?") {
		t.Errorf("with query_rewrite off the answer prompt %q does not ask the raw question", lastPrompt)
	}
	embeddingRequests := tr.provider.Requests(modeltest.Embeddings)
	if inputs := embeddingInputs(t, embeddingRequests[len(embeddingRequests)-1].Body); !slices.Equal(inputs, []string{"và năm 2024 thì sao?"}) {
		t.Errorf("with query_rewrite off dense embedded %v, want the raw follow-up", inputs)
	}

	resp = tr.get(t, "/conversations/"+plainID, alice)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /conversations/%s = %d %s, want 200", plainID, resp.StatusCode, body)
	}
	// Decode into a fresh value: reusing the earlier one would keep a
	// rewritten_query the JSON leaves out.
	var plainConversation conversationView
	if err := json.NewDecoder(resp.Body).Decode(&plainConversation); err != nil {
		t.Fatalf("decode GET /conversations: %v", err)
	}
	if len(plainConversation.Data.Turns) != 2 || plainConversation.Data.Turns[1].RewrittenQuery != nil {
		t.Errorf("rewrite-off turns = %+v, want two turns and no rewritten query", plainConversation.Data.Turns)
	}

	// Deleting the one source Document makes every turn generated from it
	// withheld: the metadata stays, the answer and rewritten query do not.
	deleted := tr.deleteDocument(t, alice, docID)
	if deleted.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(deleted.Body)
		t.Fatalf("DELETE /documents/%s = %d %s, want 204", docID, deleted.StatusCode, body)
	}
	resp = tr.get(t, "/conversations/"+conversationID, alice)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /conversations/%s after deleting the source = %d %s, want 200",
			conversationID, resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "source_document_ids") {
		t.Errorf("GET /conversations body %s exposes source_document_ids", body)
	}
	var withheldConversation conversationView
	if err := json.Unmarshal(body, &withheldConversation); err != nil {
		t.Fatalf("decode GET /conversations: %v", err)
	}
	if len(withheldConversation.Data.Turns) != 2 {
		t.Fatalf("turns = %+v, want the two turns still listed", withheldConversation.Data.Turns)
	}
	for i, turn := range withheldConversation.Data.Turns {
		if !turn.Withheld || turn.Answer != "" || turn.RewrittenQuery != nil {
			t.Errorf("turn %d = %+v, want withheld with no answer or rewritten query", i+1, turn)
		}
		if turn.Seq != int64(i+1) || turn.Question == "" || turn.CreatedAt.IsZero() {
			t.Errorf("turn %d metadata = %+v, want seq, question and created_at kept", i+1, turn)
		}
	}

	// A withheld turn is also left out of Conversation History: with no
	// readable earlier answer, the follow-up has no history, so query_rewrite
	// makes no model call, and with nothing readable retrieved the answer
	// makes no chat call either.
	before = len(tr.provider.Requests(modeltest.ChatCompletions))
	afterDelete := tr.askInConversation(t, alice, conversationID, map[string]any{
		"query":            "và năm 2024 thì sao?",
		"retrieval_config": techniques,
	})
	if step := findRewriteStep(afterDelete.Data.Debug.Trace); step != nil {
		t.Errorf("a follow-up over withheld history rewrote anyway: %+v", step)
	}
	if afterDelete.Data.Answer != expectedNoSourcesVietnamese {
		t.Errorf("answer after the source was deleted = %q, want the fixed no-source sentence", afterDelete.Data.Answer)
	}
	if after := len(tr.provider.Requests(modeltest.ChatCompletions)); after != before {
		t.Errorf("a follow-up over withheld history made %d chat calls, want none", after-before)
	}
}
