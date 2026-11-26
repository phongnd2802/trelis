package openairesponses

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
	"github.com/phongnd2802/trelis/internal/models/modeltest"
	"github.com/phongnd2802/trelis/internal/types"
)

// The fake provider answers /v1/responses from a scripted reply and records
// what each request carried, so these tests read the wire on both sides.

const (
	model = "gpt-5"
	key   = "sk-resp"
)

func clientFor(p *modeltest.FakeProvider) *Client {
	return New(Config{
		Endpoint: api.Endpoint{BaseURL: p.URL(), Model: model, Auth: api.BearerAuth(key)},
		Settings: api.DefaultOpenAIResponses(),
	})
}

// requestBody decodes the body of the i-th request the provider received.
func requestBody(t *testing.T, p *modeltest.FakeProvider, i int) map[string]any {
	t.Helper()
	reqs := p.Requests(modeltest.Responses)
	if len(reqs) <= i {
		t.Fatalf("provider saw %d requests, want at least %d", len(reqs), i+1)
	}
	var body map[string]any
	if err := json.Unmarshal(reqs[i].Body, &body); err != nil {
		t.Fatalf("decode request body %s: %v", reqs[i].Body, err)
	}
	return body
}

// drain reads a stream to the end, returning the answer text and the closing
// chunk.
func drain(t *testing.T, ch <-chan types.StreamResponse) (answer string, last types.StreamResponse) {
	t.Helper()
	for chunk := range ch {
		if chunk.ResponseType == types.ResponseTypeAnswer {
			answer += chunk.Content
		}
		if chunk.ResponseType == types.ResponseTypeError {
			t.Errorf("unexpected error chunk: %s", chunk.Content)
		}
		last = chunk
	}
	if !last.Done {
		t.Fatalf("stream closed without a Done chunk (last %+v)", last)
	}
	return answer, last
}

var question = []api.Message{{Role: "user", Content: "Capital of Vietnam?"}}

func TestChat_ReturnsTextAndUsage(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.Any(), modeltest.Reply{Text: "Hà Nội", Usage: modeltest.Usage{Input: 12, Output: 3}})
	c := clientFor(p)

	resp, err := c.Chat(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "Hà Nội" || resp.FinishReason != "stop" {
		t.Errorf("resp = %+v, want Hà Nội / stop", resp)
	}
	if want := (types.TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}); resp.Usage != want {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, want)
	}
	if c.GetModelName() != model {
		t.Errorf("GetModelName = %q, want %q", c.GetModelName(), model)
	}

	reqs := p.Requests(modeltest.Responses)
	if len(reqs) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(reqs))
	}
	if auth := reqs[0].Header.Get("Authorization"); auth != "Bearer "+key {
		t.Errorf("Authorization = %q, want Bearer %s", auth, key)
	}
	body := requestBody(t, p, 0)
	if body["model"] != model || body["store"] != false {
		t.Errorf("body = %v, want model %q and store false", body, model)
	}
	if _, streaming := body["stream"]; streaming {
		t.Errorf("non-streaming request sent stream = %v", body["stream"])
	}
}

func TestChat_SendsInstructionsAndTheToolHistory(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.All(modeltest.LastRole("tool"), modeltest.Contains(`chunk_search {"query":"Luật 2024"}`)),
		modeltest.Text("Điều 1 quy định về đất đai."))
	c := clientFor(p)
	messages := []api.Message{
		{Role: "system", Content: "Answer from the corpus."},
		{Role: "system", Content: "Be brief."},
		{Role: "user", Content: "Luật 2024 nói gì?"},
		{Role: "assistant", Content: "Checking.", ToolCalls: []api.ToolCall{{
			ID: "call_1", Type: "function",
			Function: api.FunctionCall{Name: "chunk_search", Arguments: `{"query":"Luật 2024"}`},
		}}},
		{Role: "tool", ToolCallID: "call_1", Name: "chunk_search", Content: "Điều 1."},
	}

	resp, err := c.Chat(context.Background(), messages, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "Điều 1 quy định về đất đai." {
		t.Errorf("Content = %q", resp.Content)
	}
	body := requestBody(t, p, 0)
	// Several system messages become one instructions string, the way a model
	// reads them as a single preamble.
	if got, want := body["instructions"], "Answer from the corpus.\n\nBe brief."; got != want {
		t.Errorf("instructions = %v, want %q", got, want)
	}
	want := []any{
		map[string]any{"type": "message", "role": "user", "content": "Luật 2024 nói gì?"},
		map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "Checking."}}},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "chunk_search",
			"arguments": `{"query":"Luật 2024"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "Điều 1."},
	}
	if got := body["input"]; !reflect.DeepEqual(got, want) {
		t.Errorf("input =\n%#v\nwant\n%#v", got, want)
	}
}

func TestChat_ParsesToolCallsAndSendsTheResponsesToolShape(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.HasTool("chunk_search"), modeltest.Reply{ToolCalls: []modeltest.ToolCall{{
		ID: "call_9", Name: "chunk_search", Arguments: `{"query":"x"}`,
	}}})
	c := clientFor(p)
	parallel := true

	resp, err := c.Chat(context.Background(), question, &api.Options{
		Tools: []api.Tool{searchTool}, ToolChoice: "chunk_search",
		ParallelToolCalls: &parallel, MaxTokens: 500,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	want := []types.LLMToolCall{{ID: "call_9", Type: "function",
		Function: types.FunctionCall{Name: "chunk_search", Arguments: `{"query":"x"}`}}}
	if !reflect.DeepEqual(resp.ToolCalls, want) {
		t.Errorf("ToolCalls = %+v, want %+v", resp.ToolCalls, want)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}

	body := requestBody(t, p, 0)
	if body["max_output_tokens"] != float64(500) || body["parallel_tool_calls"] != true {
		t.Errorf("body = %v, want the budget and parallel tool calls", body)
	}
	if got, want := body["tool_choice"], map[string]any{"type": "function", "name": "chunk_search"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tool_choice = %#v, want %#v", got, want)
	}
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want one entry", body["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "chunk_search" ||
		tool["description"] != searchTool.Function.Description {
		t.Errorf("tool = %#v, want the flat Responses shape", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Error("tool carries a nested function object, want the flat Responses shape")
	}
	if tool["parameters"] == nil {
		t.Error("tool carries no parameters")
	}
}

// stubClient answers every /responses request with reply, for the reply shapes
// the fake provider does not produce on its own.
func stubClient(t *testing.T, reply string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return New(Config{
		Endpoint: api.Endpoint{BaseURL: srv.URL + "/v1", Model: model},
		Settings: api.DefaultOpenAIResponses(),
	})
}

func TestChat_IncompleteIsLength(t *testing.T) {
	c := stubClient(t, `{
		"status": "incomplete",
		"incomplete_details": {"reason": "max_output_tokens"},
		"output": [{"type": "message", "content": [{"type": "output_text", "text": "partial"}]}],
		"usage": {"input_tokens": 1, "output_tokens": 2}
	}`)

	resp, err := c.Chat(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "partial" || resp.FinishReason != "length" {
		t.Errorf("resp = %+v, want the partial answer with finish reason length", resp)
	}
	if resp.Usage.TotalTokens != 3 {
		t.Errorf("Usage.TotalTokens = %d, want input plus output when the vendor omits the total", resp.Usage.TotalTokens)
	}
}

func TestChat_ErrorObjectAndFailedStatusAreErrors(t *testing.T) {
	for name, reply := range map[string]string{
		"error object": `{"error":{"type":"invalid_request_error","message":"bad model"}}`,
		"failed":       `{"status":"failed","error":{"message":"boom"}}`,
		"failed bare":  `{"status":"failed"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := stubClient(t, reply).Chat(context.Background(), question, nil); err == nil {
				t.Fatal("Chat succeeded, want an error")
			}
		})
	}
}

func TestChatStream_DeltasTextAndFinalUsage(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	text := "Xin chào thế giới hôm nay"
	p.Reply(modeltest.Any(), modeltest.Reply{Text: text, Usage: modeltest.Usage{Input: 5, Output: 9}})
	c := clientFor(p)

	ch, err := c.ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	answer, last := drain(t, ch)
	if answer != text {
		t.Errorf("answer = %q, want %q", answer, text)
	}
	if last.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", last.FinishReason)
	}
	if want := (types.TokenUsage{PromptTokens: 5, CompletionTokens: 9, TotalTokens: 14}); last.Usage == nil || *last.Usage != want {
		t.Errorf("Usage = %+v, want %+v", last.Usage, want)
	}
	if body := requestBody(t, p, 0); body["stream"] != true {
		t.Errorf("stream = %v, want true", body["stream"])
	}
}

func TestChatStream_AssemblesToolCalls(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.HasTool("chunk_search"), modeltest.Reply{
		ToolCalls: []modeltest.ToolCall{{
			ID: "call_7", Name: "chunk_search", Arguments: `{"query":"Luật 2024"}`,
		}},
		Usage: modeltest.Usage{Input: 30, Output: 20},
	})
	c := clientFor(p)

	ch, err := c.ChatStream(context.Background(), question, &api.Options{Tools: []api.Tool{searchTool}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	announced := false
	var last types.StreamResponse
	for chunk := range ch {
		if chunk.ResponseType == types.ResponseTypeToolCall && chunk.Data["tool_name"] == "chunk_search" {
			announced = true
		}
		if chunk.ResponseType == types.ResponseTypeError {
			t.Fatalf("unexpected error chunk: %s", chunk.Content)
		}
		last = chunk
	}
	if !announced {
		t.Error("no tool_call chunk announced chunk_search")
	}
	want := []types.LLMToolCall{{ID: "call_7", Type: "function",
		Function: types.FunctionCall{Name: "chunk_search", Arguments: `{"query":"Luật 2024"}`}}}
	if !reflect.DeepEqual(last.ToolCalls, want) {
		t.Errorf("ToolCalls = %+v, want %+v", last.ToolCalls, want)
	}
	if last.FinishReason != "tool_calls" || last.Usage == nil || last.Usage.TotalTokens != 50 {
		t.Errorf("last = %+v, want tool_calls with 50 tokens", last)
	}
}

func TestChatAndStream_FailedCallReturnsHTTPError(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.FailNext(modeltest.Responses, http.StatusTooManyRequests, 2)
	c := clientFor(p)

	_, chatErr := c.Chat(context.Background(), question, nil)
	_, streamErr := c.ChatStream(context.Background(), question, nil)
	for name, err := range map[string]error{"Chat": chatErr, "ChatStream": streamErr} {
		var httpErr *api.HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
			t.Errorf("%s err = %v, want *api.HTTPError with 429", name, err)
		}
	}
}

func TestChatStream_FailedEventEndsWithAnErrorChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"partial"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"status":"failed","error":{"message":"boom"}}}`+"\n\n")
	}))
	t.Cleanup(srv.Close)
	c := New(Config{
		Endpoint: api.Endpoint{BaseURL: srv.URL + "/v1", Model: model},
		Settings: api.DefaultOpenAIResponses(),
	})

	ch, err := c.ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var last types.StreamResponse
	for chunk := range ch {
		last = chunk
	}
	if last.ResponseType != types.ResponseTypeError || !strings.Contains(last.Content, "boom") {
		t.Errorf("last = %+v, want an error chunk naming the provider's message", last)
	}
}

// sseStubClient streams events, each written as its own SSE data line, for
// the events the fake provider does not produce on its own.
func sseStubClient(t *testing.T, events ...string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = io.WriteString(w, "data: "+e+"\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return New(Config{
		Endpoint: api.Endpoint{BaseURL: srv.URL + "/v1", Model: model},
		Settings: api.DefaultOpenAIResponses(),
	})
}

// response.incomplete is the terminal event for a cut-off stream: it carries
// the usage and must map to the same finish reason as the non-streaming path.
func TestChatStream_IncompleteIsTerminal(t *testing.T) {
	cases := map[string]string{
		"max_output_tokens": `{"type":"response.incomplete","response":{"status":"incomplete",` +
			`"incomplete_details":{"reason":"max_output_tokens"},` +
			`"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`,
		"no details": `{"type":"response.incomplete","response":{"status":"incomplete",` +
			`"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`,
	}
	for name, incomplete := range cases {
		t.Run(name, func(t *testing.T) {
			c := sseStubClient(t,
				`{"type":"response.output_text.delta","delta":"partial"}`,
				incomplete,
			)
			ch, err := c.ChatStream(context.Background(), question, nil)
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			answer, last := drain(t, ch)
			if answer != "partial" {
				t.Errorf("answer = %q, want partial", answer)
			}
			if last.FinishReason != "length" {
				t.Errorf("FinishReason = %q, want length", last.FinishReason)
			}
			if want := (types.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}); last.Usage == nil || *last.Usage != want {
				t.Errorf("Usage = %+v, want %+v", last.Usage, want)
			}
		})
	}
}

// Some gateways never stream argument deltas; the arguments arrive whole on a
// done event instead. Taking them only once keeps the call intact without
// appending them a second time when deltas were seen.
func TestChatStream_ToolArgumentsOnDoneEvents(t *testing.T) {
	const (
		added = `{"type":"response.output_item.added","output_index":0,"item":` +
			`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"chunk_search","arguments":""}}`
		delta = `{"type":"response.function_call_arguments.delta","item_id":"fc_1",` +
			`"output_index":0,"delta":"{\"query\":\"Luật 2024\"}"}`
		argsDone = `{"type":"response.function_call_arguments.done","item_id":"fc_1",` +
			`"output_index":0,"arguments":"{\"query\":\"Luật 2024\"}"}`
		itemDone = `{"type":"response.output_item.done","output_index":0,"item":` +
			`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"chunk_search",` +
			`"arguments":"{\"query\":\"Luật 2024\"}","status":"completed"}}`
		completed = `{"type":"response.completed","response":{"status":"completed","output":[],` +
			`"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`
	)
	cases := map[string][]string{
		"arguments done event":   {added, argsDone, completed},
		"output item done event": {added, itemDone, completed},
		"deltas and done events": {added, delta, argsDone, itemDone, completed},
	}
	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			ch, err := sseStubClient(t, events...).ChatStream(context.Background(), question, nil)
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			var last types.StreamResponse
			for chunk := range ch {
				last = chunk
			}
			want := []types.LLMToolCall{{ID: "call_1", Type: "function",
				Function: types.FunctionCall{Name: "chunk_search", Arguments: `{"query":"Luật 2024"}`}}}
			if !reflect.DeepEqual(last.ToolCalls, want) {
				t.Errorf("ToolCalls = %+v, want %+v", last.ToolCalls, want)
			}
		})
	}
}

func TestChat_RefusalIsAnswerContent(t *testing.T) {
	c := stubClient(t, `{"status":"completed","output":[{"type":"message","role":"assistant",`+
		`"content":[{"type":"refusal","refusal":"I can't help with that."}]}],`+
		`"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)

	resp, err := c.Chat(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "I can't help with that." || resp.FinishReason != "stop" {
		t.Errorf("resp = %+v, want the refusal text as the answer", resp)
	}
}

func TestChatStream_RefusalDeltasAreAnswerContent(t *testing.T) {
	c := sseStubClient(t,
		`{"type":"response.refusal.delta","delta":"I can't "}`,
		`{"type":"response.refusal.delta","delta":"help with that."}`,
		`{"type":"response.completed","response":{"status":"completed","output":[],`+
			`"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
	)
	ch, err := c.ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	answer, last := drain(t, ch)
	if answer != "I can't help with that." || last.FinishReason != "stop" {
		t.Errorf("answer/finish = %q/%q, want the refusal text with stop", answer, last.FinishReason)
	}
}

// A frame that is not JSON at all is a hole in the answer, not an event the
// client may skip: skipping would run to EOF and reach the caller as a
// successful but short reply.
func TestChatStream_BrokenStreamEndsWithAnErrorChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"Hel`+"\n\n")
	}))
	t.Cleanup(srv.Close)
	c := New(Config{
		Endpoint: api.Endpoint{BaseURL: srv.URL + "/v1", Model: model},
		Settings: api.DefaultOpenAIResponses(),
	})

	ch, err := c.ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var last types.StreamResponse
	for chunk := range ch {
		last = chunk
	}
	if last.ResponseType != types.ResponseTypeError || !strings.Contains(last.Content, "decode stream chunk") {
		t.Errorf("last = %+v, want an error chunk from decoding the broken frame", last)
	}
}
