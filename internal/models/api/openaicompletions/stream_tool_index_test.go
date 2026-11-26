// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/openaicompletions/stream_tool_index_test.go,
// with testify assertions replaced by the standard library.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

package openaicompletions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/types"
)

// streamCalls runs the given SSE data lines through ChatStream and returns the
// tool calls of the final chunk.
func streamCalls(t *testing.T, lines ...string) []types.LLMToolCall {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			_, _ = w.Write([]byte("data: " + l + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	c := New(Config{
		Endpoint: api.Endpoint{BaseURL: server.URL, Model: "m"}, Settings: api.DefaultOpenAICompletions(),
	})
	ch, err := c.ChatStream(context.Background(), []api.Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var last types.StreamResponse
	for chunk := range ch {
		if chunk.ResponseType == types.ResponseTypeError {
			t.Fatalf("error chunk: %s", chunk.Content)
		}
		if chunk.Done {
			last = chunk
		}
	}
	return last.ToolCalls
}

func toolChunk(calls string, finish string) string {
	f := "null"
	if finish != "" {
		f = `"` + finish + `"`
	}
	return `{"choices":[{"delta":{"tool_calls":[` + calls + `]},"finish_reason":` + f + `}]}`
}

// wantCalls compares id-less (name, arguments) pairs, or full calls when ids
// are given.
func wantCalls(t *testing.T, got []types.LLMToolCall, want ...types.LLMToolCall) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d calls %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if want[i].ID != "" && got[i].ID != want[i].ID {
			t.Errorf("call %d id = %q, want %q", i, got[i].ID, want[i].ID)
		}
		if got[i].Function != want[i].Function {
			t.Errorf("call %d = %+v, want %+v", i, got[i].Function, want[i].Function)
		}
	}
}

func call(id, name, args string) types.LLMToolCall {
	return types.LLMToolCall{ID: id, Function: types.FunctionCall{Name: name, Arguments: args}}
}

func TestChatStream_IndexedToolCallsStayApart(t *testing.T) {
	calls := streamCalls(t,
		toolChunk(`{"index":0,"id":"a","type":"function","function":{"name":"get_weather","arguments":""}}`, ""),
		toolChunk(`{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}`, ""),
		toolChunk(
			`{"index":1,"id":"b","type":"function","function":{"name":"get_time","arguments":"{}"}}`,
			"tool_calls"),
	)
	wantCalls(t, calls, call("a", "get_weather", `{"city":"Paris"}`), call("b", "get_time", `{}`))
}

func TestChatStream_ParallelCallsWithoutIndexStayApart(t *testing.T) {
	// Each call arrives whole in its own chunk and no chunk has an index.
	calls := streamCalls(t,
		toolChunk(
			`{"id":"a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}`,
			""),
		toolChunk(
			`{"id":"b","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}`,
			"tool_calls"),
	)
	wantCalls(t, calls, call("a", "get_weather", `{"city":"Paris"}`), call("b", "get_time", `{"tz":"UTC"}`))
}

func TestChatStream_TwoCallsInOneChunkWithoutIndexStayApart(t *testing.T) {
	calls := streamCalls(t,
		toolChunk(
			`{"id":"a","type":"function","function":{"name":"get_weather","arguments":"{}"}},`+
				`{"id":"b","type":"function","function":{"name":"get_time","arguments":"{}"}}`,
			"tool_calls"),
	)
	wantCalls(t, calls, call("a", "get_weather", `{}`), call("b", "get_time", `{}`))
}

func TestChatStream_FragmentsWithoutIndexContinueTheCall(t *testing.T) {
	// Name and id first, then argument fragments with neither, or with the
	// id repeated.
	calls := streamCalls(t,
		toolChunk(`{"id":"a","type":"function","function":{"name":"get_weather","arguments":""}}`, ""),
		toolChunk(`{"function":{"arguments":"{\"city\":"}}`, ""),
		toolChunk(`{"id":"a","function":{"arguments":"\"Paris\"}"}}`, "tool_calls"),
	)
	wantCalls(t, calls, call("a", "get_weather", `{"city":"Paris"}`))
}
