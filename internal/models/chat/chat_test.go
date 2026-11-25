package chat

import (
	"errors"
	"testing"
)

func TestNewChatRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]*ChatConfig{
		"nil config":     nil,
		"no base URL":    {ModelName: "gpt-4o-mini"},
		"no model name":  {BaseURL: "http://localhost:4000/v1"},
		"blank base URL": {BaseURL: "   ", ModelName: "gpt-4o-mini"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := NewChat(cfg)
			if err == nil {
				t.Fatalf("NewChat(%+v) = %v, want an error", cfg, c)
			}
			if errors.Is(err, ErrNotImplemented) {
				t.Fatalf("NewChat(%+v) returned ErrNotImplemented, want a config error", cfg)
			}
		})
	}
}

func TestNewChatWithValidConfigIsNotImplementedYet(t *testing.T) {
	_, err := NewChat(&ChatConfig{BaseURL: "http://localhost:4000/v1", ModelName: "gpt-4o-mini"})
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("NewChat err = %v, want ErrNotImplemented", err)
	}
}
