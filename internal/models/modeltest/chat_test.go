package modeltest_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

func TestChatCompletionsReturnsScriptedToolCalls(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.LastRole("tool"), modeltest.Text("It is sunny."))
	p.Reply(modeltest.HasTool("weather"), modeltest.Call("weather", `{"city":"Hanoi"}`))

	body := chatBody("Weather in Hanoi?")
	body["tools"] = []map[string]any{{"type": "function", "function": map[string]any{"name": "weather", "parameters": map[string]any{"type": "object"}}}}
	_, raw := post(t, p, "/chat/completions", body)
	c := decode[chatCompletion](t, raw).Choices[0]
	if c.FinishReason != "tool_calls" || c.Message.Content != nil || len(c.Message.ToolCalls) != 1 {
		t.Fatalf("choice = %s, want one tool call and null content", raw)
	}
	if !strings.Contains(string(raw), `"content":null`) {
		t.Errorf("answer %s leaves content out instead of null", raw)
	}
	call := c.Message.ToolCalls[0]
	if call.ID == "" || call.Type != "function" || call.Function.Name != "weather" || call.Function.Arguments != `{"city":"Hanoi"}` {
		t.Errorf("tool call = %+v", call)
	}

	body["messages"] = append(body["messages"].([]map[string]any),
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{"id": call.ID, "type": "function", "function": map[string]any{"name": "weather", "arguments": `{"city":"Hanoi"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": call.ID, "content": "sunny, 31C"},
	)
	_, raw = post(t, p, "/chat/completions", body)
	if got := decode[chatCompletion](t, raw).Choices[0].Message.Content; got == nil || *got != "It is sunny." {
		t.Errorf("after the tool result: %s", raw)
	}
}

func TestChatCompletionsReadsContentParts(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.LastContains("second part"), modeltest.Text("ok"))
	body := map[string]any{"model": "m", "messages": []map[string]any{{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": "first part"}, {"type": "text", "text": "second part"}},
	}}}
	if status, raw := post(t, p, "/chat/completions", body); status != http.StatusOK {
		t.Errorf("status = %d %s", status, raw)
	}
}

// events reads an SSE body into its data payloads, in order.
func events(t *testing.T, raw []byte) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			out = append(out, data)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no SSE events in %q", raw)
	}
	return out
}

func TestChatCompletionsStreamsTextToolCallsAndUsage(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.Any(), modeltest.Reply{
		Text:      "Let me check the weather for you.",
		ToolCalls: []modeltest.ToolCall{{ID: "call_w", Name: "weather", Arguments: `{"city": "Hanoi", "unit": "C"}`}},
		Usage:     modeltest.Usage{Input: 12, Output: 7},
	})
	body := chatBody("Weather in Hanoi?")
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}

	status, raw := post(t, p, "/chat/completions", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d %s", status, raw)
	}
	data := events(t, raw)
	if data[len(data)-1] != "[DONE]" {
		t.Errorf("last event = %q, want [DONE]", data[len(data)-1])
	}
	var (
		text, args, name, id, finish string
		deltas                       int
		usage                        chatCompletion
	)
	for _, d := range data[:len(data)-1] {
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(d), &chunk); err != nil {
			t.Fatalf("chunk %s: %v", d, err)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("object = %q", chunk.Object)
		}
		if len(chunk.Choices) == 0 {
			usage = decode[chatCompletion](t, []byte(d))
			continue
		}
		c := chunk.Choices[0]
		if c.Delta.Content != "" {
			deltas++
		}
		text += c.Delta.Content
		for _, tc := range c.Delta.ToolCalls {
			id += tc.ID
			name += tc.Function.Name
			args += tc.Function.Arguments
		}
		if c.FinishReason != nil {
			finish = *c.FinishReason
		}
	}
	if text != "Let me check the weather for you." || deltas < 2 {
		t.Errorf("text = %q in %d deltas, want the reply in several pieces", text, deltas)
	}
	if id != "call_w" || name != "weather" || args != `{"city": "Hanoi", "unit": "C"}` {
		t.Errorf("tool call = %q %q %q", id, name, args)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q", finish)
	}
	if usage.Usage.PromptTokens != 12 || usage.Usage.CompletionTokens != 7 || usage.Usage.TotalTokens != 19 {
		t.Errorf("usage chunk = %+v", usage.Usage)
	}
}

// recorder is a testing.TB that records Errorf instead of failing.
type recorder struct {
	testing.TB
	errors []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func TestAnUnscriptedGenerationRequestFailsTheTest(t *testing.T) {
	rec := &recorder{TB: t}
	p := modeltest.NewFakeProvider(rec)
	p.Reply(modeltest.LastContains("France"), modeltest.Text("Paris."))

	status, raw := post(t, p, "/chat/completions", chatBody("Capital of Spain?"))
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
	if !strings.Contains(string(raw), "Capital of Spain?") {
		t.Errorf("error body %s does not show the request", raw)
	}
	if len(rec.errors) != 1 || !strings.Contains(rec.errors[0], "Capital of Spain?") {
		t.Errorf("test errors = %q, want one naming the request", rec.errors)
	}
}
