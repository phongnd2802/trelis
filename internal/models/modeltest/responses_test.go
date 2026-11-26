package modeltest_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

type outputItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type response struct {
	Object string       `json:"object"`
	Status string       `json:"status"`
	Model  string       `json:"model"`
	Output []outputItem `json:"output"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func TestResponsesAnswersFromTheSameScriptAsChat(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.All(modeltest.LastRole("tool"), modeltest.Contains("be brief")), modeltest.Text("Sunny."))
	p.Reply(modeltest.HasTool("weather"), modeltest.Reply{
		Text:      "Checking.",
		ToolCalls: []modeltest.ToolCall{{ID: "call_1", Name: "weather", Arguments: `{"city":"Hanoi"}`}},
	})

	body := map[string]any{
		"model":        "gpt-test",
		"instructions": "be brief",
		"input":        "Weather in Hanoi?",
		"tools":        []map[string]any{{"type": "function", "name": "weather", "parameters": map[string]any{"type": "object"}}},
	}
	status, raw := post(t, p, "/responses", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d %s", status, raw)
	}
	got := decode[response](t, raw)
	if got.Object != "response" || got.Status != "completed" || got.Model != "gpt-test" {
		t.Errorf("response = %s", raw)
	}
	if len(got.Output) != 2 {
		t.Fatalf("output = %s, want a message and a function call", raw)
	}
	msg, call := got.Output[0], got.Output[1]
	if msg.Type != "message" || msg.Role != "assistant" || len(msg.Content) != 1 ||
		msg.Content[0].Type != "output_text" || msg.Content[0].Text != "Checking." {
		t.Errorf("message = %+v", msg)
	}
	if call.Type != "function_call" || call.CallID != "call_1" || call.Name != "weather" || call.Arguments != `{"city":"Hanoi"}` {
		t.Errorf("function call = %+v", call)
	}
	if got.Usage.InputTokens == 0 || got.Usage.TotalTokens != got.Usage.InputTokens+got.Usage.OutputTokens {
		t.Errorf("usage = %+v", got.Usage)
	}

	body["input"] = []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "Weather in Hanoi?"}}},
		{"type": "function_call", "call_id": "call_1", "name": "weather", "arguments": `{"city":"Hanoi"}`},
		{"type": "function_call_output", "call_id": "call_1", "output": "sunny"},
	}
	_, raw = post(t, p, "/responses", body)
	if out := decode[response](t, raw).Output; len(out) != 1 || out[0].Content[0].Text != "Sunny." {
		t.Errorf("after the tool output: %s", raw)
	}
}

func TestToolCallHistoryMatchesTheSameOnBothEndpoints(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.Contains(`weather {"city":"Hue"}`), modeltest.Text("Rainy."))

	chat := map[string]any{"model": "m", "messages": []map[string]any{
		{"role": "user", "content": "Weather?"},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "weather", "arguments": `{"city":"Hue"}`}}}},
		{"role": "tool", "tool_call_id": "c1", "content": "rain"},
	}}
	responses := map[string]any{"model": "m", "input": []map[string]any{
		{"role": "user", "content": "Weather?"},
		{"type": "function_call", "call_id": "c1", "name": "weather", "arguments": `{"city":"Hue"}`},
		{"type": "function_call_output", "call_id": "c1", "output": "rain"},
	}}
	if status, raw := post(t, p, "/chat/completions", chat); status != http.StatusOK {
		t.Errorf("chat: %d %s", status, raw)
	}
	if status, raw := post(t, p, "/responses", responses); status != http.StatusOK {
		t.Errorf("responses: %d %s", status, raw)
	}
}

func TestResponsesStreamsTextAndFunctionCallEvents(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.Any(), modeltest.Reply{
		Text:      "Let me check the weather for you.",
		ToolCalls: []modeltest.ToolCall{{ID: "call_1", Name: "weather", Arguments: `{"city": "Hanoi", "unit": "C"}`}},
		Usage:     modeltest.Usage{Input: 5, Output: 9},
	})
	status, raw := post(t, p, "/responses", map[string]any{"model": "gpt-test", "input": "Weather?", "stream": true})
	if status != http.StatusOK {
		t.Fatalf("status = %d %s", status, raw)
	}

	var (
		types      []string
		text, args string
		textDeltas int
		done       []outputItem
		completed  response
		lastSeqNum = -1
	)
	for _, d := range events(t, raw) {
		var e struct {
			Type           string          `json:"type"`
			SequenceNumber int             `json:"sequence_number"`
			Delta          string          `json:"delta"`
			Item           outputItem      `json:"item"`
			Response       json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal([]byte(d), &e); err != nil {
			t.Fatalf("event %s: %v", d, err)
		}
		if e.SequenceNumber <= lastSeqNum {
			t.Errorf("sequence_number %d after %d", e.SequenceNumber, lastSeqNum)
		}
		lastSeqNum = e.SequenceNumber
		types = append(types, e.Type)
		switch e.Type {
		case "response.output_text.delta":
			text += e.Delta
			textDeltas++
		case "response.function_call_arguments.delta":
			args += e.Delta
		case "response.output_item.done":
			done = append(done, e.Item)
		case "response.completed":
			completed = decode[response](t, e.Response)
		}
	}
	if types[0] != "response.created" || types[len(types)-1] != "response.completed" {
		t.Errorf("event types = %v, want response.created first and response.completed last", types)
	}
	if text != "Let me check the weather for you." || textDeltas < 2 {
		t.Errorf("text = %q in %d deltas", text, textDeltas)
	}
	if args != `{"city": "Hanoi", "unit": "C"}` {
		t.Errorf("arguments = %q", args)
	}
	if len(done) != 2 || done[1].Type != "function_call" || done[1].Name != "weather" || done[1].CallID != "call_1" {
		t.Errorf("finished items = %+v", done)
	}
	if completed.Status != "completed" || len(completed.Output) != 2 || completed.Usage.InputTokens != 5 || completed.Usage.OutputTokens != 9 {
		t.Errorf("completed response = %+v", completed)
	}
}
