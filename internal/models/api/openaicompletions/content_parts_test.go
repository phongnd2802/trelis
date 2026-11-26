// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/openaicompletions/content_parts_test.go
// and the stream-failure cases of golden_test.go, with testify assertions
// replaced by the standard library.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

package openaicompletions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/phongnd2802/trelis/internal/types"
)

func TestMessageContent_StringOrParts(t *testing.T) {
	cases := []struct {
		name, in, text, thinking string
	}{
		{"string", `"hi"`, "hi", ""},
		{"null", `null`, "", ""},
		{"text parts", `[{"type":"text","text":"Hel"},{"type":"text","text":"lo"}]`, "Hello", ""},
		{
			"mistral thinking part",
			`[{"type":"thinking","thinking":[{"type":"text","text":"let me see"}]},{"type":"text","text":"answer"}]`,
			"answer", "let me see",
		},
		{
			"reference part carries no text",
			`[{"type":"reference","reference_ids":[1]},{"type":"text","text":"a"}]`,
			"a", "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m rawMessage
			if err := json.Unmarshal([]byte(`{"content":`+tc.in+`}`), &m); err != nil {
				t.Fatal(err)
			}
			if m.Content.Text != tc.text || m.Content.Thinking != tc.thinking {
				t.Errorf("text/thinking = %q/%q, want %q/%q", m.Content.Text, m.Content.Thinking, tc.text, tc.thinking)
			}
		})
	}
}

// streamText collects thinking, answer and error text from a stream.
func streamText(t *testing.T, ch <-chan types.StreamResponse) (thinking, answer, errText string) {
	t.Helper()
	for chunk := range ch {
		switch chunk.ResponseType {
		case types.ResponseTypeError:
			errText += chunk.Content
		case types.ResponseTypeThinking:
			thinking += chunk.Content
		case types.ResponseTypeAnswer:
			answer += chunk.Content
		}
	}
	return thinking, answer, errText
}

func rawSSE(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

func openStream(t *testing.T, respond func(http.ResponseWriter)) <-chan types.StreamResponse {
	t.Helper()
	p := newFakeProvider(t, respond)
	ch, err := p.client("sk").ChatStream(context.Background(), question, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	return ch
}

// Mistral types delta.content as string | ContentChunk[]. A chunk sent as a
// list of parts used to fail the whole stream with "cannot unmarshal array".
func TestChatStream_ContentPartsAreDecoded(t *testing.T) {
	ch := openStream(t, sseReply(
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"plan"}]}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":[{"type":"text","text":"Hel"}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	))
	thinking, answer, errText := streamText(t, ch)
	if errText != "" {
		t.Fatalf("unexpected error: %s", errText)
	}
	if thinking != "plan" || answer != "Hello" {
		t.Errorf("thinking/answer = %q/%q, want plan/Hello", thinking, answer)
	}
}

func TestChat_NonStreamContentParts(t *testing.T) {
	p := newFakeProvider(t, jsonReply(`{"choices":[{"index":0,"message":{"role":"assistant","content":`+
		`[{"type":"thinking","thinking":[{"type":"text","text":"deep"}]},{"type":"text","text":"answer"}]},`+
		`"finish_reason":"stop"}]}`))
	resp, err := p.client("sk").Chat(context.Background(), question, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "answer" || resp.ReasoningContent != "deep" {
		t.Errorf("content/reasoning = %q/%q, want answer/deep", resp.Content, resp.ReasoningContent)
	}
}

func TestChatStream_TolerantDoneSentinel(t *testing.T) {
	ch := openStream(t, rawSSE(
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"+
			"data:[DONE]\n\n"))
	_, answer, errText := streamText(t, ch)
	if errText != "" || answer != "hi" {
		t.Errorf("answer/error = %q/%q, want hi and no error", answer, errText)
	}
}

// A chunk that will not decode is a hole in the answer. Skipping it would end
// the stream on EOF, so a proxy that corrupted one packet would produce a
// truncated reply recorded as a successful one.
func TestChatStream_UndecodableChunkFailsTheStream(t *testing.T) {
	ch := openStream(t, rawSSE(
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n"+
			"data: <html>502 Bad Gateway</html>\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n"))
	_, answer, errText := streamText(t, ch)
	if !strings.Contains(errText, "decode stream chunk") {
		t.Errorf("error = %q, want a decode error: a truncated stream must not pass as a short answer", errText)
	}
	if answer != "Hel" {
		t.Errorf("answer = %q, want Hel: decoding stops at the bad chunk", answer)
	}
}

// A tool_calls entry that cannot be decoded must not turn a round that wanted
// to call a tool into a plain answer with nothing to act on.
func TestChat_MalformedToolCallIsAnError(t *testing.T) {
	p := newFakeProvider(t, jsonReply(`{"choices":[{"index":0,"message":{"role":"assistant","content":"",`+
		`"tool_calls":["not-an-object"]},"finish_reason":"tool_calls"}]}`))
	_, err := p.client("sk").Chat(context.Background(), question, nil)
	if err == nil || !strings.Contains(err.Error(), "decode tool call") {
		t.Errorf("err = %v, want a decode tool call error", err)
	}
}

// The streaming counterpart: the round must end as an error rather than as a
// short answer whose tool call quietly vanished.
func TestChatStream_MalformedToolCallFailsTheStream(t *testing.T) {
	ch := openStream(t, rawSSE(
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[42]}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"+
			"data: [DONE]\n\n"))
	_, answer, errText := streamText(t, ch)
	if !strings.Contains(errText, "decode tool call") {
		t.Errorf("error = %q, want a decode tool call error", errText)
	}
	if answer != "ok" {
		t.Errorf("answer = %q, want ok: decoding stops at the bad chunk", answer)
	}
}
