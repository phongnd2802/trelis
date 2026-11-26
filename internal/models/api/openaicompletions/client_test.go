package openaicompletions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/types"
)

// fakeProvider is an OpenAI-compatible endpoint that records the last request
// and answers with respond.
type fakeProvider struct {
	srv      *httptest.Server
	lastPath string
	lastAuth string
	lastBody map[string]any
}

func newFakeProvider(t *testing.T, respond func(w http.ResponseWriter)) *fakeProvider {
	t.Helper()
	p := &fakeProvider{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.lastPath = r.URL.Path
		p.lastAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		p.lastBody = nil
		if err := json.Unmarshal(raw, &p.lastBody); err != nil {
			t.Errorf("request body is not JSON: %v: %s", err, raw)
		}
		respond(w)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProvider) client(key string) *Client {
	return New(Config{
		Endpoint: api.Endpoint{BaseURL: p.srv.URL + "/v1", Model: "m", Auth: api.BearerAuth(key)},
		Settings: api.DefaultOpenAICompletions(),
	})
}

func jsonReply(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// sseReply streams each event as one SSE data line, flushing between them.
func sseReply(events ...string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = io.WriteString(w, "data: "+e+"\n\n")
			w.(http.Flusher).Flush()
		}
	}
}

// drain reads a stream to the end, returning the answer text and the closing
// chunk.
func drain(t *testing.T, ch <-chan types.StreamResponse) (answer string, last types.StreamResponse) {
	t.Helper()
	for chunk := range ch {
		if chunk.ResponseType == types.ResponseTypeAnswer {
			answer += chunk.Content
		}
		last = chunk
	}
	if !last.Done {
		t.Fatalf("stream closed without a Done chunk (last %+v)", last)
	}
	return answer, last
}

var question = []api.Message{{Role: "user", Content: "Capital of Vietnam?"}}

func TestChat_ReturnsContentAndUsage(t *testing.T) {
	p := newFakeProvider(t, jsonReply(`{
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "Hà Nội"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}
	}`))

	resp, err := p.client("sk-test").Chat(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "Hà Nội" || resp.FinishReason != "stop" {
		t.Errorf("resp = %+v, want Hà Nội / stop", resp)
	}
	if want := (types.TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}); resp.Usage != want {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, want)
	}
	if p.lastPath != "/v1/chat/completions" || p.lastAuth != "Bearer sk-test" {
		t.Errorf("path/auth = %q/%q", p.lastPath, p.lastAuth)
	}
	if _, ok := p.lastBody["stream"]; ok {
		t.Errorf("non-streaming request sent stream = %v", p.lastBody["stream"])
	}
}

func TestChat_NoKeySendsNoAuthorization(t *testing.T) {
	p := newFakeProvider(t, jsonReply(`{"choices":[{"message":{"content":"x"}}]}`))
	if _, err := p.client("").Chat(context.Background(), question, nil); err != nil {
		t.Fatal(err)
	}
	if p.lastAuth != "" {
		t.Errorf("Authorization = %q, want none", p.lastAuth)
	}
}

func TestChat_ParsesToolCalls(t *testing.T) {
	p := newFakeProvider(t, jsonReply(`{
		"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
			"role": "assistant", "content": null,
			"tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "chunk_search", "arguments": "{\"query\":\"Nghị định 15\"}"}}]
		}}],
		"usage": {"prompt_tokens": 40, "completion_tokens": 9}
	}`))

	resp, err := p.client("").Chat(context.Background(), question, &api.Options{Tools: []api.Tool{searchTool}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	want := []types.LLMToolCall{{ID: "call_1", Type: "function",
		Function: types.FunctionCall{Name: "chunk_search", Arguments: `{"query":"Nghị định 15"}`}}}
	if !reflect.DeepEqual(resp.ToolCalls, want) {
		t.Errorf("ToolCalls = %+v, want %+v", resp.ToolCalls, want)
	}
	if resp.Usage.TotalTokens != 49 {
		t.Errorf("TotalTokens = %d, want 49 (prompt + completion when the vendor omits it)", resp.Usage.TotalTokens)
	}
	if p.lastBody["tools"] == nil {
		t.Error("request sent no tools")
	}
}

func TestChat_SplitsInlineThinkTags(t *testing.T) {
	p := newFakeProvider(t, jsonReply(`{"choices":[{"message":{"content":"<think>recall the law</think>Luật 2024"}}]}`))
	resp, err := p.client("").Chat(context.Background(), question, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Luật 2024" || resp.ReasoningContent != "recall the law" {
		t.Errorf("Content/Reasoning = %q/%q", resp.Content, resp.ReasoningContent)
	}
}

func TestChatStream_DeltasReasoningAndUsage(t *testing.T) {
	p := newFakeProvider(t, sseReply(
		`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hà "}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Nội"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`,
		`[DONE]`,
	))

	ch, err := p.client("").ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var thinking string
	var answer string
	var last types.StreamResponse
	for chunk := range ch {
		switch chunk.ResponseType {
		case types.ResponseTypeThinking:
			thinking += chunk.Content
		case types.ResponseTypeAnswer:
			answer += chunk.Content
		}
		last = chunk
	}

	if thinking != "think" || answer != "Hà Nội" {
		t.Errorf("thinking/answer = %q/%q", thinking, answer)
	}
	if !last.Done || last.FinishReason != "stop" {
		t.Errorf("last = %+v, want Done with stop", last)
	}
	if want := (types.TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}); last.Usage == nil || *last.Usage != want {
		t.Errorf("Usage = %+v, want %+v", last.Usage, want)
	}
	if p.lastBody["stream"] != true {
		t.Errorf("stream = %v, want true", p.lastBody["stream"])
	}
}

func TestChatStream_AnnouncesAndAssemblesToolCalls(t *testing.T) {
	p := newFakeProvider(t, sseReply(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"chunk_search","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Luật 2024\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":20,"total_tokens":50}}`,
		`[DONE]`,
	))

	ch, err := p.client("").ChatStream(context.Background(), question, &api.Options{Tools: []api.Tool{searchTool}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	announced := false
	var last types.StreamResponse
	for chunk := range ch {
		if chunk.ResponseType == types.ResponseTypeToolCall && chunk.Data["tool_name"] == "chunk_search" {
			announced = true
		}
		last = chunk
	}
	if !announced {
		t.Error("no tool_call chunk announced chunk_search")
	}
	wantCalls(t, last.ToolCalls, call("call_a", "chunk_search", `{"query":"Luật 2024"}`))
	if last.FinishReason != "tool_calls" || last.Usage == nil || last.Usage.TotalTokens != 50 {
		t.Errorf("last = %+v, want tool_calls with 50 tokens", last)
	}
}

func TestChatStream_CutOffIsIncomplete(t *testing.T) {
	p := newFakeProvider(t, sseReply(`{"choices":[{"index":0,"delta":{"content":"Hà"}}]}`))

	ch, err := p.client("").ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	answer, last := drain(t, ch)
	if answer != "Hà" || last.FinishReason != types.FinishReasonIncomplete {
		t.Errorf("answer/finish = %q/%q, want Hà/%s", answer, last.FinishReason, types.FinishReasonIncomplete)
	}
}

func TestChatStream_ProviderErrorEventClosesWithError(t *testing.T) {
	p := newFakeProvider(t, sseReply(`{"error":{"message":"overloaded"}}`))

	ch, err := p.client("").ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	_, last := drain(t, ch)
	if last.ResponseType != types.ResponseTypeError || !strings.Contains(last.Content, "overloaded") {
		t.Errorf("last = %+v, want an error chunk naming the provider's message", last)
	}
}

func TestChatAndStream_FailedCallReturnsHTTPError(t *testing.T) {
	p := newFakeProvider(t, func(w http.ResponseWriter) {
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	})
	c := p.client("")

	_, chatErr := c.Chat(context.Background(), question, nil)
	_, streamErr := c.ChatStream(context.Background(), question, nil)
	for name, err := range map[string]error{"Chat": chatErr, "ChatStream": streamErr} {
		var httpErr *api.HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
			t.Errorf("%s err = %v, want *api.HTTPError with 429", name, err)
			continue
		}
		if strings.Contains(err.Error(), "rate limited") {
			t.Errorf("%s err = %v, want the body kept out of the message", name, err)
		}
		if !strings.Contains(httpErr.Body, "rate limited") {
			t.Errorf("%s body = %q, want the provider's message", name, httpErr.Body)
		}
	}
}

func TestChatStream_StopsWhenContextIsCancelled(t *testing.T) {
	release := make(chan struct{})
	p := newFakeProvider(t, func(w http.ResponseWriter) {
		sseReply(`{"choices":[{"index":0,"delta":{"content":"a"}}]}`)(w)
		<-release
	})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.client("").ChatStream(ctx, question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if chunk := <-ch; chunk.Content != "a" {
		t.Fatalf("first chunk = %+v, want content a", chunk)
	}
	cancel()
	for range ch { // must close without the server finishing
	}
}
