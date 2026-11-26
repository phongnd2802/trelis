package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phongnd2802/trelis/internal/config"
	"github.com/phongnd2802/trelis/internal/models/api"
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

	chatRole, err := NewChat(role(chatSrv.URL, "gen-model", "key-chat"))
	if err != nil {
		t.Fatal(err)
	}
	judgeRole, err := NewChat(role(judgeSrv.URL, "judge-model", "key-judge"))
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
			c, err := NewChat(tc.role)
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
