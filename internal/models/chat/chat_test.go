package chat

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

	"github.com/phongnd2802/trelis/internal/config"
	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/types"
)

// fakeProvider is an OpenAI-compatible endpoint that records the last request
// and answers with a fixed body.
type fakeProvider struct {
	t        *testing.T
	srv      *httptest.Server
	lastPath string
	lastAuth string
	lastBody map[string]any
}

func newFakeProvider(t *testing.T, respond func(w http.ResponseWriter)) *fakeProvider {
	t.Helper()
	p := &fakeProvider{t: t}
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

func jsonReply(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func mustNew(t *testing.T, cfg Config) Chat {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestChatReturnsContentAndUsage(t *testing.T) {
	p := newFakeProvider(t, jsonReply(`{
		"id": "chatcmpl-1",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "Hà Nội"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}
	}`))
	c := mustNew(t, Config{BaseURL: p.srv.URL + "/v1", Model: "gpt-4o-mini", APIKey: "sk-test"})

	resp, err := c.Chat(context.Background(), []types.ChatMessage{
		{Role: types.RoleSystem, Content: "Answer briefly."},
		{Role: types.RoleUser, Content: "Capital of Vietnam?"},
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if resp.Content != "Hà Nội" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hà Nội")
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", resp.FinishReason)
	}
	if want := (types.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}); resp.Usage != want {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, want)
	}
	if p.lastPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", p.lastPath)
	}
	if p.lastAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", p.lastAuth)
	}
	if p.lastBody["model"] != "gpt-4o-mini" {
		t.Errorf("model = %v, want gpt-4o-mini", p.lastBody["model"])
	}
	msgs, _ := p.lastBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2", p.lastBody["messages"])
	}
	if m := msgs[1].(map[string]any); m["role"] != "user" || m["content"] != "Capital of Vietnam?" {
		t.Errorf("messages[1] = %v, want the user question", m)
	}
	if _, ok := p.lastBody["stream"]; ok {
		t.Errorf("non-streaming request sent stream = %v", p.lastBody["stream"])
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

// collect drains a stream, returning the deltas and the final event.
func collect(t *testing.T, ch <-chan types.StreamingResponse) (content string, last types.StreamingResponse) {
	t.Helper()
	n := 0
	for ev := range ch {
		if last.Done {
			t.Fatalf("event after Done: %+v", ev)
		}
		content += ev.Content
		last = ev
		n++
	}
	if n == 0 || !last.Done {
		t.Fatalf("stream ended without a Done event (last %+v)", last)
	}
	return content, last
}

func TestChatStreamDeltasAndUsage(t *testing.T) {
	p := newFakeProvider(t, sseReply(
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hà "}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Nội"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`,
		`[DONE]`,
	))
	c := mustNew(t, Config{BaseURL: p.srv.URL, Model: "m"})

	ch, err := c.ChatStream(context.Background(), []types.ChatMessage{{Role: types.RoleUser, Content: "q"}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	content, last := collect(t, ch)

	if content != "Hà Nội" {
		t.Errorf("content = %q, want %q", content, "Hà Nội")
	}
	if last.Err != nil {
		t.Errorf("Err = %v", last.Err)
	}
	if last.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", last.FinishReason)
	}
	if want := (types.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}); last.Usage != want {
		t.Errorf("Usage = %+v, want %+v", last.Usage, want)
	}
	if p.lastBody["stream"] != true {
		t.Errorf("stream = %v, want true", p.lastBody["stream"])
	}
	if want := map[string]any{"include_usage": true}; !reflect.DeepEqual(p.lastBody["stream_options"], want) {
		t.Errorf("stream_options = %v, want %v", p.lastBody["stream_options"], want)
	}
}

func TestChatStreamAssemblesToolCalls(t *testing.T) {
	p := newFakeProvider(t, sseReply(
		`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"chunk_search","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"graph_query","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Luật 2024\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":20,"total_tokens":50}}`,
		`[DONE]`,
	))
	c := mustNew(t, Config{BaseURL: p.srv.URL, Model: "m"})

	ch, err := c.ChatStream(context.Background(), []types.ChatMessage{{Role: types.RoleUser, Content: "q"}},
		&types.ChatOptions{Tools: []types.Tool{searchTool}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	_, last := collect(t, ch)

	want := []types.ToolCall{
		{ID: "call_a", Name: "chunk_search", Arguments: `{"query":"Luật 2024"}`},
		{ID: "call_b", Name: "graph_query", Arguments: `{}`},
	}
	if !reflect.DeepEqual(last.ToolCalls, want) {
		t.Errorf("ToolCalls = %+v, want %+v", last.ToolCalls, want)
	}
	if last.FinishReason != "tool_calls" || last.Usage.TotalTokens != 50 || last.Err != nil {
		t.Errorf("final = %+v, want tool_calls, 50 tokens, no error", last)
	}
	if p.lastBody["tools"] == nil {
		t.Error("streaming request sent no tools")
	}
}

func TestChatStreamCutOffReportsAnError(t *testing.T) {
	p := newFakeProvider(t, sseReply(`{"choices":[{"index":0,"delta":{"content":"Hà"}}]}`))
	c := mustNew(t, Config{BaseURL: p.srv.URL, Model: "m"})

	ch, err := c.ChatStream(context.Background(), []types.ChatMessage{{Role: types.RoleUser, Content: "q"}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	_, last := collect(t, ch)
	if !errors.Is(last.Err, io.ErrUnexpectedEOF) {
		t.Errorf("Err = %v, want io.ErrUnexpectedEOF", last.Err)
	}
}

func TestChatStreamProviderErrorEvent(t *testing.T) {
	p := newFakeProvider(t, sseReply(`{"error":{"message":"overloaded"}}`))
	c := mustNew(t, Config{BaseURL: p.srv.URL, Model: "m"})

	ch, err := c.ChatStream(context.Background(), []types.ChatMessage{{Role: types.RoleUser, Content: "q"}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	_, last := collect(t, ch)
	if last.Err == nil || !strings.Contains(last.Err.Error(), "overloaded") {
		t.Errorf("Err = %v, want the provider's message", last.Err)
	}
}

func TestFailedCallReturnsTheStatus(t *testing.T) {
	p := newFakeProvider(t, func(w http.ResponseWriter) {
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	})
	c := mustNew(t, Config{BaseURL: p.srv.URL, Model: "m"})
	msgs := []types.ChatMessage{{Role: types.RoleUser, Content: "q"}}

	_, chatErr := c.Chat(context.Background(), msgs, nil)
	_, streamErr := c.ChatStream(context.Background(), msgs, nil)
	for name, err := range map[string]error{"Chat": chatErr, "ChatStream": streamErr} {
		var httpErr *api.HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
			t.Errorf("%s err = %v, want *api.HTTPError with 429", name, err)
			continue
		}
		if !strings.Contains(err.Error(), "rate limited") {
			t.Errorf("%s err = %v, want the provider's message", name, err)
		}
	}
}

func TestChatStreamStopsWhenContextIsCancelled(t *testing.T) {
	release := make(chan struct{})
	p := newFakeProvider(t, func(w http.ResponseWriter) {
		sseReply(`{"choices":[{"index":0,"delta":{"content":"a"}}]}`)(w)
		<-release
	})
	defer close(release)
	c := mustNew(t, Config{BaseURL: p.srv.URL, Model: "m"})

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.ChatStream(ctx, []types.ChatMessage{{Role: types.RoleUser, Content: "q"}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if ev := <-ch; ev.Content != "a" {
		t.Fatalf("first event = %+v, want content a", ev)
	}
	cancel()
	for range ch { // must close without the server finishing
	}
}

func TestEachRoleCallsItsOwnProvider(t *testing.T) {
	chatP := newFakeProvider(t, jsonReply(`{"choices":[{"message":{"content":"from chat"}}],"usage":{"total_tokens":1}}`))
	judgeP := newFakeProvider(t, jsonReply(`{"choices":[{"message":{"content":"from judge"}}],"usage":{"total_tokens":2}}`))
	chatRole := mustNew(t, Config{BaseURL: chatP.srv.URL, Model: "gen-model", APIKey: "key-chat"})
	judgeRole := mustNew(t, Config{BaseURL: judgeP.srv.URL, Model: "judge-model", APIKey: "key-judge"})
	msgs := []types.ChatMessage{{Role: types.RoleUser, Content: "q"}}

	got, err := judgeRole.Chat(context.Background(), msgs, nil)
	if err != nil || got.Content != "from judge" {
		t.Fatalf("judge Chat = %+v, %v", got, err)
	}
	got, err = chatRole.Chat(context.Background(), msgs, nil)
	if err != nil || got.Content != "from chat" {
		t.Fatalf("chat Chat = %+v, %v", got, err)
	}
	if judgeP.lastBody["model"] != "judge-model" || judgeP.lastAuth != "Bearer key-judge" {
		t.Errorf("judge provider got model %v auth %q", judgeP.lastBody["model"], judgeP.lastAuth)
	}
	if chatP.lastBody["model"] != "gen-model" || chatP.lastAuth != "Bearer key-chat" {
		t.Errorf("chat provider got model %v auth %q", chatP.lastBody["model"], chatP.lastAuth)
	}
	if chatRole.ModelName() != "gen-model" || judgeRole.ModelName() != "judge-model" {
		t.Errorf("ModelName = %q / %q", chatRole.ModelName(), judgeRole.ModelName())
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]Config{
		"no base URL":      {Model: "gpt-4o-mini"},
		"no model":         {BaseURL: "http://localhost:4000/v1"},
		"blank base URL":   {BaseURL: "   ", Model: "gpt-4o-mini"},
		"unknown protocol": {BaseURL: "http://localhost:4000/v1", Model: "m", Protocol: "carrier_pigeon"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if c, err := New(cfg); err == nil {
				t.Fatalf("New(%+v) = %v, want an error", cfg, c)
			}
		})
	}
}

func TestFromRoleCarriesTheRoleEndpoint(t *testing.T) {
	p := newFakeProvider(t, jsonReply(`{"choices":[{"message":{"content":"x"}}]}`))
	role := config.ChatRole{Protocol: "chat_completions", Endpoint: config.Endpoint{BaseURL: p.srv.URL, Model: "judge-model", APIKey: "k"}}

	c := mustNew(t, FromRole(role))
	if _, err := c.Chat(context.Background(), []types.ChatMessage{{Role: types.RoleUser, Content: "q"}}, nil); err != nil {
		t.Fatal(err)
	}
	if p.lastBody["model"] != "judge-model" || p.lastAuth != "Bearer k" {
		t.Errorf("provider got model %v auth %q", p.lastBody["model"], p.lastAuth)
	}
}

func TestNoKeySendsNoAuthorization(t *testing.T) {
	p := newFakeProvider(t, jsonReply(`{"choices":[{"message":{"content":"x"}}]}`))
	c := mustNew(t, Config{BaseURL: p.srv.URL, Model: "m"})
	if _, err := c.Chat(context.Background(), []types.ChatMessage{{Role: types.RoleUser, Content: "q"}}, nil); err != nil {
		t.Fatal(err)
	}
	if p.lastAuth != "" {
		t.Errorf("Authorization = %q, want none", p.lastAuth)
	}
}

var searchTool = types.Tool{
	Name:        "chunk_search",
	Description: "Search the corpus.",
	Parameters: map[string]any{
		"type":       "object",
		"properties": map[string]any{"query": map[string]any{"type": "string"}},
		"required":   []any{"query"},
	},
}

func TestChatSendsToolsAndParsesToolCalls(t *testing.T) {
	p := newFakeProvider(t, jsonReply(`{
		"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
			"role": "assistant", "content": null,
			"tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "chunk_search", "arguments": "{\"query\":\"Nghị định 15\"}"}}]
		}}],
		"usage": {"prompt_tokens": 40, "completion_tokens": 9, "total_tokens": 49}
	}`))
	c := mustNew(t, Config{BaseURL: p.srv.URL, Model: "m"})

	resp, err := c.Chat(context.Background(), []types.ChatMessage{
		{Role: types.RoleUser, Content: "What amends Decree 15?"},
		{Role: types.RoleAssistant, ToolCalls: []types.ToolCall{{ID: "call_0", Name: "chunk_search", Arguments: `{"query":"Decree 15"}`}}},
		{Role: types.RoleTool, ToolCallID: "call_0", Content: "no hits"},
	}, &types.ChatOptions{Tools: []types.Tool{searchTool}, ToolChoice: "auto"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	want := []types.ToolCall{{ID: "call_1", Name: "chunk_search", Arguments: `{"query":"Nghị định 15"}`}}
	if !reflect.DeepEqual(resp.ToolCalls, want) {
		t.Errorf("ToolCalls = %+v, want %+v", resp.ToolCalls, want)
	}
	if resp.FinishReason != "tool_calls" || resp.Usage.TotalTokens != 49 {
		t.Errorf("FinishReason/Usage = %q/%+v, want tool_calls/49 total", resp.FinishReason, resp.Usage)
	}

	wantTools := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "chunk_search",
			"description": "Search the corpus.",
			"parameters":  searchTool.Parameters,
		},
	}}
	if !reflect.DeepEqual(p.lastBody["tools"], wantTools) {
		t.Errorf("tools = %#v, want %#v", p.lastBody["tools"], wantTools)
	}
	if p.lastBody["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto", p.lastBody["tool_choice"])
	}
	msgs := p.lastBody["messages"].([]any)
	wantAssistant := map[string]any{
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{map[string]any{
			"id": "call_0", "type": "function",
			"function": map[string]any{"name": "chunk_search", "arguments": `{"query":"Decree 15"}`},
		}},
	}
	if !reflect.DeepEqual(msgs[1], wantAssistant) {
		t.Errorf("assistant message = %#v, want %#v", msgs[1], wantAssistant)
	}
	wantTool := map[string]any{"role": "tool", "content": "no hits", "tool_call_id": "call_0"}
	if !reflect.DeepEqual(msgs[2], wantTool) {
		t.Errorf("tool message = %#v, want %#v", msgs[2], wantTool)
	}
}

func TestChatForcesANamedTool(t *testing.T) {
	p := newFakeProvider(t, jsonReply(`{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}]}`))
	c := mustNew(t, Config{BaseURL: p.srv.URL, Model: "m"})

	if _, err := c.Chat(context.Background(), []types.ChatMessage{{Role: types.RoleUser, Content: "q"}},
		&types.ChatOptions{Tools: []types.Tool{searchTool}, ToolChoice: "chunk_search"}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	want := map[string]any{"type": "function", "function": map[string]any{"name": "chunk_search"}}
	if !reflect.DeepEqual(p.lastBody["tool_choice"], want) {
		t.Errorf("tool_choice = %#v, want %#v", p.lastBody["tool_choice"], want)
	}
}
