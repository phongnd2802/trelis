package openairesponses

import (
	"context"
	"reflect"
	"testing"

	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/models/modeltest"
	"github.com/phongnd2802/trelis/internal/types"
)

// response.completed carries the usage; the closing chunk is the only Done
// chunk and must carry it.
func TestChatStream_CompletedClosesOnceWithUsage(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.Any(), modeltest.Reply{Text: "Xin chào", Usage: modeltest.Usage{Input: 5, Output: 9}})
	c := clientFor(p)

	ch, err := c.ChatStream(context.Background(), question, nil)
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
	if answer != "Xin chào" {
		t.Errorf("answer = %q, want Xin chào", answer)
	}
	if idx != len(chunks)-1 {
		t.Errorf("closing chunk at %d of %d, want the last chunk: %+v", idx, len(chunks), chunks)
	}
	if closing.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", closing.FinishReason)
	}
	if want := (types.TokenUsage{PromptTokens: 5, CompletionTokens: 9, TotalTokens: 14}); closing.Usage == nil || *closing.Usage != want {
		t.Errorf("Usage = %+v, want %+v", closing.Usage, want)
	}
}

// response.incomplete is terminal too: one closing chunk with the mapped
// finish reason and the usage.
func TestChatStream_IncompleteClosesOnceWithUsage(t *testing.T) {
	c := sseStubClient(t,
		`{"type":"response.output_text.delta","delta":"partial"}`,
		`{"type":"response.incomplete","response":{"status":"incomplete",`+
			`"incomplete_details":{"reason":"max_output_tokens"},`+
			`"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`,
	)
	ch, err := c.ChatStream(context.Background(), question, nil)
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
	if answer != "partial" {
		t.Errorf("answer = %q, want partial", answer)
	}
	if closing.FinishReason != "length" {
		t.Errorf("FinishReason = %q, want length", closing.FinishReason)
	}
	if want := (types.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}); closing.Usage == nil || *closing.Usage != want {
		t.Errorf("Usage = %+v, want %+v", closing.Usage, want)
	}
}

// A Responses stream that ends without response.completed or
// response.incomplete leaves no finish reason. Both a bare EOF and a [DONE]
// sentinel close it once, marked incomplete.
func TestChatStream_EOFWithoutATerminalEventClosesOnceIncomplete(t *testing.T) {
	cases := map[string][]string{
		"EOF": {`{"type":"response.output_text.delta","delta":"partial"}`},
		"[DONE]": {
			`{"type":"response.output_text.delta","delta":"partial"}`,
			`[DONE]`,
		},
	}
	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			ch, err := sseStubClient(t, events...).ChatStream(context.Background(), question, nil)
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			chunks := modeltest.Collect(t, ch)
			idx := modeltest.CloseIndex(t, chunks)
			if idx != len(chunks)-1 {
				t.Errorf("closing chunk at %d of %d, want the last chunk: %+v", idx, len(chunks), chunks)
			}
			closing := chunks[idx]
			if closing.FinishReason != types.FinishReasonIncomplete {
				t.Errorf("FinishReason = %q, want %s", closing.FinishReason, types.FinishReasonIncomplete)
			}
			var answer string
			for _, chunk := range chunks {
				if chunk.ResponseType == types.ResponseTypeAnswer {
					answer += chunk.Content
				}
			}
			if answer != "partial" {
				t.Errorf("answer = %q, want partial", answer)
			}
		})
	}
}

// The Responses protocol must attach tool calls to the closing chunk only, as
// the Completions client does.
func TestChatStream_ToolCallsRideOnlyTheClosingChunk(t *testing.T) {
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
	chunks := modeltest.Collect(t, ch)
	idx := modeltest.CloseIndex(t, chunks)
	closing := chunks[idx]

	want := []types.LLMToolCall{{ID: "call_7", Type: "function",
		Function: types.FunctionCall{Name: "chunk_search", Arguments: `{"query":"Luật 2024"}`}}}
	if !reflect.DeepEqual(closing.ToolCalls, want) {
		t.Errorf("ToolCalls = %+v, want %+v", closing.ToolCalls, want)
	}
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
