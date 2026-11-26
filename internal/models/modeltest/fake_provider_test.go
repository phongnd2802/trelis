package modeltest_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

// post sends body as JSON to the provider and returns the status and raw response.
func post(t *testing.T, p *modeltest.FakeProvider, path string, body any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, p.URL()+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

type chatCompletion struct {
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role      string  `json:"role"`
			Content   *string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func chatBody(user string) map[string]any {
	return map[string]any{
		"model":    "gpt-test",
		"messages": []map[string]any{{"role": "system", "content": "be brief"}, {"role": "user", "content": user}},
	}
}

func TestScriptReplaysRepliesInOrderAcrossBothProtocols(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Script(modeltest.Contains("tax rules"), modeltest.Call("search", `{"q":"tax"}`), modeltest.Text("Done."))
	p.Reply(modeltest.Any(), modeltest.Text("unrelated"))

	// A request the script's Match rejects does not consume it.
	_, raw := post(t, p, "/chat/completions", chatBody("Something else"))
	if got := decode[chatCompletion](t, raw).Choices[0].Message.Content; got == nil || *got != "unrelated" {
		t.Errorf("unrelated call = %s, want the rule's reply", raw)
	}
	_, raw = post(t, p, "/chat/completions", chatBody("Find the tax rules"))
	if c := decode[chatCompletion](t, raw).Choices[0]; len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].Function.Name != "search" {
		t.Errorf("first call = %s, want the search tool call", raw)
	}
	_, raw = post(t, p, "/responses", map[string]any{"model": "m", "input": "Find the tax rules"})
	if out := decode[response](t, raw).Output; len(out) != 1 || out[0].Content[0].Text != "Done." {
		t.Errorf("second call = %s, want Done.", raw)
	}
	if n := p.CallCount(modeltest.ChatCompletions) + p.CallCount(modeltest.Responses); n != 3 {
		t.Errorf("call count = %d, want 3", n)
	}
}

func TestAnUnexpectedExtraCallAfterTheScriptFailsTheTest(t *testing.T) {
	rec := &recorder{TB: t}
	p := modeltest.NewFakeProvider(rec)
	p.Script(modeltest.Any(), modeltest.Text("first"))

	_, raw := post(t, p, "/chat/completions", chatBody("hi"))
	if got := decode[chatCompletion](t, raw).Choices[0].Message.Content; got == nil || *got != "first" {
		t.Errorf("scripted reply = %s", raw)
	}
	if status, _ := post(t, p, "/chat/completions", chatBody("hi")); status != http.StatusInternalServerError {
		t.Errorf("extra call status = %d, want 500", status)
	}
	if len(rec.errors) != 1 {
		t.Errorf("test errors = %q, want one for the extra call", rec.errors)
	}
}

func TestChatCompletionsAnswersTheFirstMatchingScriptedReply(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.LastContains("capital of France"), modeltest.Text("Paris."))
	p.Reply(modeltest.Any(), modeltest.Text("I don't know."))

	status, raw := post(t, p, "/chat/completions", chatBody("What is the capital of France?"))
	if status != http.StatusOK {
		t.Fatalf("status = %d %s", status, raw)
	}
	got := decode[chatCompletion](t, raw)
	if got.Object != "chat.completion" || got.Model != "gpt-test" {
		t.Errorf("object, model = %q, %q", got.Object, got.Model)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(got.Choices))
	}
	c := got.Choices[0]
	if c.Message.Role != "assistant" || c.Message.Content == nil || *c.Message.Content != "Paris." || c.FinishReason != "stop" {
		t.Errorf("choice = %+v", c)
	}
	if got.Usage.PromptTokens == 0 || got.Usage.CompletionTokens == 0 ||
		got.Usage.TotalTokens != got.Usage.PromptTokens+got.Usage.CompletionTokens {
		t.Errorf("usage = %+v, want non-zero and summed", got.Usage)
	}

	_, raw = post(t, p, "/chat/completions", chatBody("And of Spain?"))
	if got := decode[chatCompletion](t, raw).Choices[0].Message.Content; got == nil || *got != "I don't know." {
		t.Errorf("fallback reply = %v", got)
	}
}
