package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/phongnd2802/trelis/internal/types"
)

type Message struct {
}

type ChatOptions struct {
}

type Chat interface {
	Chat(ctx context.Context, messages []Message, opts *ChatOptions) (*types.ChatResponse, error)
	ChatStream(ctx context.Context, messages []Message, opts *ChatOptions) (<-chan types.StreamingResponse, error)

	ModelName() string
	ModelID() string
}

type ChatConfig struct {
	Source        types.ModelSource
	BaseURL       string
	ModelName     string
	APIKey        string
	ModelID       string
	Provider      string
	CustomHeaders map[string]string
}

// ErrNotImplemented is returned for a valid config until a protocol client
// exists (Chat Completions in #17, Responses in #19).
var ErrNotImplemented = errors.New("chat: no client implemented yet")

// NewChat builds the Chat client described by config.
func NewChat(config *ChatConfig) (Chat, error) {
	if config == nil {
		return nil, errors.New("chat: config is required")
	}
	return NewRemoteChat(config)
}

// NewRemoteChat builds a Chat client for an OpenAI-compatible endpoint.
func NewRemoteChat(config *ChatConfig) (Chat, error) {
	if strings.TrimSpace(config.BaseURL) == "" {
		return nil, errors.New("chat: base URL is required")
	}
	if strings.TrimSpace(config.ModelName) == "" {
		return nil, errors.New("chat: model name is required")
	}
	return nil, fmt.Errorf("%w: model %q at %s", ErrNotImplemented, config.ModelName, config.BaseURL)
}
