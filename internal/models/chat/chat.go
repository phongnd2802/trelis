// Package chat is the client for the chat model roles (chat, extraction,
// judge). Each role is built from its own Config, so each can point at a
// different OpenAI-compatible provider; the protocol behind it is chosen by
// Config.Protocol.
package chat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/phongnd2802/trelis/internal/config"
	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/models/api/openaichat"
	"github.com/phongnd2802/trelis/internal/types"
)

// Chat is one chat model role. Every call reports the provider's token usage.
type Chat interface {
	// Chat waits for the whole answer.
	Chat(ctx context.Context, messages []types.ChatMessage, opts *types.ChatOptions) (*types.ChatResponse, error)
	// ChatStream answers with content deltas; the last event has Done set and
	// carries the tool calls, usage and any error. The channel closes after it,
	// or when ctx is cancelled.
	ChatStream(ctx context.Context, messages []types.ChatMessage, opts *types.ChatOptions) (<-chan types.StreamingResponse, error)

	ModelName() string
}

// Protocol is the wire protocol a chat role speaks.
type Protocol string

const (
	// ProtocolChatCompletions is POST {base_url}/chat/completions.
	ProtocolChatCompletions Protocol = "chat_completions"
)

// Config describes one chat role's provider.
type Config struct {
	// Protocol defaults to ProtocolChatCompletions.
	Protocol Protocol
	// BaseURL is the API root, e.g. https://api.openai.com/v1.
	BaseURL string
	Model   string
	// APIKey is sent as a bearer token; empty sends none.
	APIKey  string
	Headers map[string]string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// FromRole is the Config for one configured chat role (models.chat,
// models.extraction or models.judge).
func FromRole(role config.ChatRole) Config {
	return Config{
		Protocol: Protocol(role.Protocol),
		BaseURL:  role.BaseURL,
		Model:    role.Model,
		APIKey:   role.APIKey,
	}
}

// New builds the Chat client described by cfg.
func New(cfg Config) (Chat, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("chat: base URL is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("chat: model is required")
	}
	endpoint := api.Endpoint{
		BaseURL: cfg.BaseURL,
		Model:   cfg.Model,
		Auth:    api.BearerAuth(cfg.APIKey),
		Headers: cfg.Headers,
		Client:  cfg.HTTPClient,
	}
	switch cfg.Protocol {
	case "", ProtocolChatCompletions:
		return &completionsChat{client: openaichat.New(endpoint), model: cfg.Model}, nil
	default:
		return nil, fmt.Errorf("chat: unknown protocol %q", cfg.Protocol)
	}
}

type completionsChat struct {
	client *openaichat.Client
	model  string
}

func (c *completionsChat) ModelName() string { return c.model }

func (c *completionsChat) Chat(ctx context.Context, messages []types.ChatMessage, opts *types.ChatOptions) (*types.ChatResponse, error) {
	resp, err := c.client.Chat(ctx, messages, deref(opts))
	if err != nil {
		return nil, fmt.Errorf("chat %s: %w", c.model, err)
	}
	return resp, nil
}

func (c *completionsChat) ChatStream(ctx context.Context, messages []types.ChatMessage, opts *types.ChatOptions) (<-chan types.StreamingResponse, error) {
	ch, err := c.client.ChatStream(ctx, messages, deref(opts))
	if err != nil {
		return nil, fmt.Errorf("chat %s: %w", c.model, err)
	}
	return ch, nil
}

func deref(opts *types.ChatOptions) types.ChatOptions {
	if opts == nil {
		return types.ChatOptions{}
	}
	return *opts
}
