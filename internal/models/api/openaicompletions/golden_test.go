package openaicompletions

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/phongnd2802/trelis/internal/models/api"
)

// Golden tests pin the exact wire body, in the style of WeKnora's
// golden_test.go: each case lists the keys it asserts, nil meaning absent.

var searchTool = api.Tool{Type: "function", Function: api.FunctionDef{
	Name: "chunk_search", Description: "Search the corpus.",
	Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
}}

var conversation = []api.Message{
	{Role: "system", Content: "Answer from the corpus."},
	{Role: "user", Content: "What amends Decree 15?"},
	{Role: "assistant", ReasoningContent: "search first", ToolCalls: []api.ToolCall{{
		ID: "call_0", Type: "function", Function: api.FunctionCall{Name: "chunk_search", Arguments: `{"query":"Decree 15"}`},
	}}},
	{Role: "tool", ToolCallID: "call_0", Name: "chunk_search", Content: "no hits"},
}

func ptrBool(b bool) *bool { return &b }

func newClient(mutate func(*Config)) *Client {
	cfg := Config{
		Endpoint: api.Endpoint{BaseURL: "https://example.com/v1", Model: "m", Auth: api.BearerAuth("k")},
		Settings: api.DefaultOpenAICompletions(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(cfg)
}

// bodyJSON round-trips the body through JSON so values compare as decoded.
func bodyJSON(t *testing.T, c *Client, msgs []api.Message, opts *api.Options, stream bool) map[string]any {
	t.Helper()
	data, err := json.Marshal(c.BuildRequestBody(msgs, opts, stream))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGolden(t *testing.T) {
	toolOpts := &api.Options{
		Temperature: 0.2, MaxTokens: 500, Tools: []api.Tool{searchTool},
		ToolChoice: "auto", ParallelToolCalls: ptrBool(true),
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		opts   *api.Options
		stream bool
		want   map[string]any
	}{
		{
			name:   "baseline stream: max_completion_tokens, usage in stream, tools",
			opts:   toolOpts,
			stream: true,
			want: map[string]any{
				"model": "m", "stream": true, "stream_options": map[string]any{"include_usage": true},
				"max_completion_tokens": float64(500), "max_tokens": nil,
				"temperature": 0.2, "tool_choice": "auto", "parallel_tool_calls": true,
				"tools": []any{map[string]any{"type": "function", "function": map[string]any{
					"name": "chunk_search", "description": "Search the corpus.",
					"parameters": map[string]any{
						"type":       "object",
						"properties": map[string]any{"query": map[string]any{"type": "string"}},
						"required":   []any{"query"},
					},
				}}},
			},
		},
		{
			name:   "non-stream sends neither stream nor stream_options",
			opts:   nil,
			stream: false,
			want:   map[string]any{"stream": nil, "stream_options": nil, "tools": nil},
		},
		{
			name:   "vendor on max_tokens",
			mutate: func(c *Config) { c.Settings.MaxTokensField = "max_tokens" },
			opts:   &api.Options{MaxCompletionTokens: 64},
			want:   map[string]any{"max_tokens": float64(64), "max_completion_tokens": nil},
		},
		{
			name: "named tool choice forces that function",
			opts: &api.Options{Tools: []api.Tool{searchTool}, ToolChoice: "chunk_search"},
			want: map[string]any{"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "chunk_search"}}},
		},
		{
			name:   "tool choice the vendor rejects is dropped",
			mutate: func(c *Config) { c.Settings.ToolChoiceModes = []string{"none", "auto"} },
			opts:   &api.Options{Tools: []api.Tool{searchTool}, ToolChoice: "required"},
			want:   map[string]any{"tool_choice": nil},
		},
		{
			name:   "no sampling for models that refuse temperature",
			mutate: func(c *Config) { c.Settings.SupportsTemperature = false },
			opts:   &api.Options{Temperature: 0.7, TopP: 0.9},
			want:   map[string]any{"temperature": nil, "top_p": nil},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bodyJSON(t, newClient(tc.mutate), conversation, tc.opts, tc.stream)
			for key, want := range tc.want {
				value, present := got[key]
				if want == nil {
					if present {
						t.Errorf("%s = %#v, want absent", key, value)
					}
					continue
				}
				if !reflect.DeepEqual(value, want) {
					t.Errorf("%s = %#v, want %#v", key, value, want)
				}
			}
		})
	}
}

func TestGolden_Messages(t *testing.T) {
	got := bodyJSON(t, newClient(nil), conversation, nil, false)["messages"]
	want := []any{
		map[string]any{"role": "system", "content": "Answer from the corpus."},
		map[string]any{"role": "user", "content": "What amends Decree 15?"},
		map[string]any{
			// Empty content is sent as "" rather than omitted: strict vLLM /
			// Qwen chat templates crash on a missing field.
			"role": "assistant", "content": "", "reasoning_content": "search first",
			"tool_calls": []any{map[string]any{
				"id": "call_0", "type": "function",
				"function": map[string]any{"name": "chunk_search", "arguments": `{"query":"Decree 15"}`},
			}},
		},
		map[string]any{"role": "tool", "content": "no hits", "tool_call_id": "call_0", "name": "chunk_search"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("messages =\n%#v\nwant\n%#v", got, want)
	}
}
