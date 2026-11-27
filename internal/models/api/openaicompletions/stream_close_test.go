package openaicompletions

import (
	"context"
	"testing"

	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/models/modeltest"
	"github.com/phongnd2802/trelis/internal/types"
)

// A provider that reports usage in its own chunk after finish_reason (OpenAI
// with stream_options.include_usage) must still produce one closing chunk,
// carrying that usage. A consumer that stops at the first Done would otherwise
// lose the token counts.
func TestChatStream_UsageAfterFinishReasonClosesOnce(t *testing.T) {
	p := newFakeProvider(t, sseReply(
		`{"choices":[{"index":0,"delta":{"content":"Hà "}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Nội."}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":106,"completion_tokens":58,"total_tokens":164}}`,
		`[DONE]`,
	))
	ch, err := p.client("").ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	chunks := modeltest.Collect(t, ch)
	idx := modeltest.CloseIndex(t, chunks)
	closing := chunks[idx]

	var answer string
	for _, chunk := range chunks {
		if chunk.ResponseType == types.ResponseTypeAnswer && chunk.Content != "" {
			answer += chunk.Content
		}
	}
	if answer != "Hà Nội." {
		t.Errorf("answer = %q, want Hà Nội.", answer)
	}
	if idx != len(chunks)-1 {
		t.Errorf("closing chunk at %d of %d, want the last chunk: %+v", idx, len(chunks), chunks)
	}
	if closing.ResponseType != types.ResponseTypeAnswer || closing.Content != "" {
		t.Errorf("closing chunk = %+v, want an empty answer chunk", closing)
	}
	if closing.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", closing.FinishReason)
	}
	if want := (types.TokenUsage{PromptTokens: 106, CompletionTokens: 58, TotalTokens: 164}); closing.Usage == nil || *closing.Usage != want {
		t.Errorf("Usage = %+v, want %+v", closing.Usage, want)
	}
}

// Tool calls belong to the closing chunk only: an agent loop that runs tools
// on a Done chunk must not see them twice.
func TestChatStream_ToolCallsRideOnlyTheClosingChunk(t *testing.T) {
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
	chunks := modeltest.Collect(t, ch)
	idx := modeltest.CloseIndex(t, chunks)
	closing := chunks[idx]

	wantCalls(t, closing.ToolCalls, call("call_a", "chunk_search", `{"query":"Luật 2024"}`))
	carriers := 0
	for i, chunk := range chunks {
		if len(chunk.ToolCalls) == 0 {
			continue
		}
		carriers++
		if i != idx {
			t.Errorf("chunk %d carries ToolCalls, want only the closing chunk %d: %+v", i, idx, chunk)
		}
	}
	if carriers != 1 {
		t.Errorf("%d chunks carry ToolCalls, want exactly one: %+v", carriers, chunks)
	}
	announced := false
	for _, chunk := range chunks {
		if chunk.ResponseType == types.ResponseTypeToolCall && chunk.Data["tool_name"] == "chunk_search" && !chunk.Done {
			announced = true
		}
	}
	if !announced {
		t.Error("no tool_call chunk announced chunk_search")
	}
	if closing.FinishReason != "tool_calls" || closing.Usage == nil || closing.Usage.TotalTokens != 50 {
		t.Errorf("closing chunk = %+v, want tool_calls with 50 tokens", closing)
	}
}

// A finish reason may ride the same delta as content. The content is a normal
// answer chunk, not the close; the closing chunk follows it.
func TestChatStream_ContentInTheFinishingDeltaIsNotTheClose(t *testing.T) {
	p := newFakeProvider(t, sseReply(
		`{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	))
	ch, err := p.client("").ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	chunks := modeltest.Collect(t, ch)
	idx := modeltest.CloseIndex(t, chunks)
	closing := chunks[idx]

	var answer string
	for _, chunk := range chunks {
		if chunk.ResponseType == types.ResponseTypeAnswer {
			answer += chunk.Content
		}
	}
	if answer != "Hello" {
		t.Errorf("answer = %q, want Hello", answer)
	}
	if closing.Content != "" || closing.FinishReason != "stop" {
		t.Errorf("closing chunk = %+v, want empty content with stop", closing)
	}
}

// A stream cut before [DONE] closes once, keeping the finish reason the
// provider already sent. Without one it is marked incomplete.
func TestChatStream_EOFAfterAFinishReasonClosesOnceWithIt(t *testing.T) {
	p := newFakeProvider(t, sseReply(
		`{"choices":[{"index":0,"delta":{"content":"Hà"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	))
	ch, err := p.client("").ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	chunks := modeltest.Collect(t, ch)
	closing := chunks[modeltest.CloseIndex(t, chunks)]

	if closing.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", closing.FinishReason)
	}
}

// The thinking-done marker comes once, before the first answer token.
func TestChatStream_ThinkingMarkerComesOnceBeforeTheAnswer(t *testing.T) {
	p := newFakeProvider(t, sseReply(
		`{"choices":[{"index":0,"delta":{"reasoning_content":"plan"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
		`[DONE]`,
	))
	ch, err := p.client("").ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	chunks := modeltest.Collect(t, ch)
	idx := modeltest.CloseIndex(t, chunks)

	markers, markerAt, firstAnswerAt := 0, -1, -1
	for i, chunk := range chunks {
		if chunk.ResponseType == types.ResponseTypeThinking && chunk.Done {
			markers++
			markerAt = i
		}
		if chunk.ResponseType == types.ResponseTypeAnswer && chunk.Content != "" && firstAnswerAt < 0 {
			firstAnswerAt = i
		}
	}
	if markers != 1 {
		t.Errorf("%d thinking-done markers, want exactly one: %+v", markers, chunks)
	}
	if markerAt < 0 || firstAnswerAt < 0 || markerAt > firstAnswerAt {
		t.Errorf("marker at %d, first answer token at %d; want the marker first: %+v",
			markerAt, firstAnswerAt, chunks)
	}
	if closing := chunks[idx]; closing.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", closing.FinishReason)
	}
}

// Reasoning with no answer text still closes its thinking exactly once, just
// before the one closing chunk.
func TestChatStream_ReasoningOnlyEndsWithMarkerThenClosing(t *testing.T) {
	p := newFakeProvider(t, sseReply(
		`{"choices":[{"index":0,"delta":{"reasoning_content":"plan"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	))
	ch, err := p.client("").ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	chunks := modeltest.Collect(t, ch)
	idx := modeltest.CloseIndex(t, chunks)

	if idx < 1 || chunks[idx-1].ResponseType != types.ResponseTypeThinking || !chunks[idx-1].Done {
		t.Errorf("chunk before the close = %+v, want the one thinking-done marker: %+v", chunks[max(idx-1, 0)], chunks)
	}
	for _, chunk := range chunks {
		if chunk.ResponseType == types.ResponseTypeAnswer && chunk.Content != "" {
			t.Errorf("answer content %q in a reasoning-only stream", chunk.Content)
		}
	}
	if closing := chunks[idx]; closing.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", closing.FinishReason)
	}
}
