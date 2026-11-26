package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/phongnd2802/trelis/internal/config"
	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

// The wire protocol is tested in internal/models/api/openaicompletions; these
// tests cover building a client from a chat role.

type seen struct {
	model, auth string
}

// provider answers every chat call with content and records what it got.
func provider(t *testing.T, content string, got *seen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		*got = seen{model: body.Model, auth: r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`+content+`"}}],"usage":{"total_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func role(baseURL, model, key string) config.ChatRole {
	return config.ChatRole{
		Protocol: api.APIOpenAICompletions,
		Endpoint: config.Endpoint{BaseURL: baseURL, Model: model, APIKey: key},
	}
}

func TestNewChat_EachRoleCallsItsOwnProvider(t *testing.T) {
	var chatSeen, judgeSeen seen
	chatSrv := provider(t, "from chat", &chatSeen)
	judgeSrv := provider(t, "from judge", &judgeSeen)

	chatRole, err := NewChat("chat", role(chatSrv.URL, "gen-model", "key-chat"))
	if err != nil {
		t.Fatal(err)
	}
	judgeRole, err := NewChat("judge", role(judgeSrv.URL, "judge-model", "key-judge"))
	if err != nil {
		t.Fatal(err)
	}
	msgs := []Message{{Role: "user", Content: "q"}}

	if got, err := judgeRole.Chat(context.Background(), msgs, nil); err != nil || got.Content != "from judge" {
		t.Fatalf("judge Chat = %+v, %v", got, err)
	}
	if got, err := chatRole.Chat(context.Background(), msgs, nil); err != nil || got.Content != "from chat" {
		t.Fatalf("chat Chat = %+v, %v", got, err)
	}
	if judgeSeen != (seen{"judge-model", "Bearer key-judge"}) {
		t.Errorf("judge provider saw %+v", judgeSeen)
	}
	if chatSeen != (seen{"gen-model", "Bearer key-chat"}) {
		t.Errorf("chat provider saw %+v", chatSeen)
	}
	if chatRole.GetModelName() != "gen-model" || judgeRole.GetModelName() != "judge-model" {
		t.Errorf("GetModelName = %q / %q", chatRole.GetModelName(), judgeRole.GetModelName())
	}
}

func TestNewChat_SwitchesProtocolWithTheRoleConfig(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.Any(), modeltest.Text("ok"))
	msgs := []Message{{Role: "user", Content: "cùng một câu hỏi"}}

	// The caller does not change; only the role's protocol does.
	ask := func(protocol api.API) string {
		r := role(p.URL(), "m", "k")
		r.Protocol = protocol
		c, err := NewChat("chat", r)
		if err != nil {
			t.Fatalf("NewChat(%s): %v", protocol, err)
		}
		resp, err := c.Chat(context.Background(), msgs, nil)
		if err != nil {
			t.Fatalf("Chat(%s): %v", protocol, err)
		}
		return resp.Content
	}

	if got := ask(api.APIOpenAIResponses); got != "ok" {
		t.Errorf("Responses Chat = %q, want ok", got)
	}
	if n := p.CallCount(modeltest.Responses); n != 1 {
		t.Errorf("provider saw %d responses requests, want 1", n)
	}
	if n := p.CallCount(modeltest.ChatCompletions); n != 0 {
		t.Errorf("provider saw %d chat completions requests, want 0", n)
	}

	if got := ask(api.APIOpenAICompletions); got != "ok" {
		t.Errorf("Completions Chat = %q, want ok", got)
	}
	if n := p.CallCount(modeltest.ChatCompletions); n != 1 {
		t.Errorf("provider saw %d chat completions requests, want 1", n)
	}
	if n := p.CallCount(modeltest.Responses); n != 1 {
		t.Errorf("provider saw %d responses requests, want 1 still", n)
	}
}

func TestNewChat_RejectsIncompleteRole(t *testing.T) {
	unknown := role("http://localhost:4000/v1", "m", "")
	unknown.Protocol = "carrier-pigeon"
	cases := map[string]struct {
		role config.ChatRole
		// want is the part of the message that says which role, or which of
		// its settings, is missing.
		want string
	}{
		"unconfigured":     {role: config.ChatRole{}, want: "chat:"},
		"no base URL":      {role: role("", "gpt-4o-mini", ""), want: "chat:"},
		"no model":         {role: role("http://localhost:4000/v1", "", ""), want: "chat:"},
		"blank base URL":   {role: role("   ", "gpt-4o-mini", ""), want: "chat:"},
		"unknown protocol": {role: unknown, want: "unsupported chat api"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := NewChat("chat", tc.role)
			if err == nil {
				t.Fatalf("NewChat(%+v) = %v, want an error", tc.role, c)
			}
			// The endpoint errors name the role, so a caller wiring several of
			// them together can tell which one is incomplete.
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NewChat err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestChatLogsOneModelCallWithoutTheMessage(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.Any(), modeltest.Reply{
		Text:  "secret answer",
		Usage: modeltest.Usage{Input: 12, Output: 7},
	})
	c, err := NewChat("chat", role(p.URL(), "m", "k"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "secret prompt"}}, nil); err != nil {
		t.Fatal(err)
	}

	entries := logs.FilterMessage("model call").All()
	if len(entries) != 1 {
		t.Fatalf("logger saw %d model call lines, want 1", len(entries))
	}
	entry := entries[0]
	fields := entry.ContextMap()
	want := map[string]any{
		"role":          "chat",
		"model":         "m",
		"protocol":      string(api.APIOpenAICompletions),
		"status":        "ok",
		"attempts":      int64(1),
		"input_tokens":  int64(12),
		"output_tokens": int64(7),
	}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("%s = %v, want %v", k, fields[k], v)
		}
	}
	if s := fmt.Sprint(entry); strings.Contains(s, "secret prompt") || strings.Contains(s, "secret answer") {
		t.Errorf("the model call log carries message content: %s", s)
	}
}

func TestFailedChatLogsOneInfoLine(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	p := modeltest.NewFakeProvider(t)
	p.FailNext(modeltest.ChatCompletions, http.StatusInternalServerError, 1)
	c, err := NewChat("chat", role(p.URL(), "m", "k"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "secret prompt"}}, nil); err == nil {
		t.Fatal("Chat succeeded, want the provider's error")
	}

	entries := logs.FilterMessage("model call").All()
	if len(entries) != 1 {
		t.Fatalf("logger saw %d model call lines, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Level != zapcore.InfoLevel {
		t.Errorf("level = %v, want INFO", entry.Level)
	}
	fields := entry.ContextMap()
	if fields["status"] != "error" || fields["http_status"] != int64(http.StatusInternalServerError) {
		t.Errorf("fields = %v, want status error and http_status 500", fields)
	}
	if _, ok := fields["error"]; ok {
		t.Errorf("fields = %v, want no error text in the model-call line", fields)
	}
	if s := fmt.Sprint(entry); strings.Contains(s, "secret prompt") {
		t.Errorf("the model call log carries the prompt: %s", s)
	}
}

func TestChatStreamLogsWhenTheStreamFinishes(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	p := modeltest.NewFakeProvider(t)
	p.Reply(modeltest.Any(), modeltest.Text("secret answer"))
	c, err := NewChat("chat", role(p.URL(), "m", "k"))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "secret prompt"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	entries := logs.FilterMessage("model call").All()
	if len(entries) != 1 {
		t.Fatalf("logger saw %d model call lines, want 1", len(entries))
	}
	entry := entries[0]
	if got := entry.ContextMap()["status"]; got != "ok" {
		t.Errorf("status = %v, want ok", got)
	}
	if s := fmt.Sprint(entry); strings.Contains(s, "secret prompt") || strings.Contains(s, "secret answer") {
		t.Errorf("the model call log carries message content: %s", s)
	}
}
