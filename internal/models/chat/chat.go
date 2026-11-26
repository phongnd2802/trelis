// Package chat is the client for the chat model roles (chat, extraction,
// judge). Each role is built from its own config.ChatRole, so each can point
// at a different OpenAI-compatible provider; the role's protocol picks the
// wire client behind it.
package chat

import (
	"context"
	"errors"
	"fmt"
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
	// carries the tool calls, usage and any error. The channel closes after
	// it, or without it once ctx is cancelled.
	ChatStream(ctx context.Context, messages []types.ChatMessage, opts *types.ChatOptions) (<-chan types.StreamingResponse, error)

	ModelName() string
}

// New builds the client for one configured chat role (models.chat,
// models.extraction or models.judge).
func New(role config.ChatRole) (Chat, error) {
	if strings.TrimSpace(role.BaseURL) == "" {
		return nil, errors.New("chat: base URL is required")
	}
	if strings.TrimSpace(role.Model) == "" {
		return nil, errors.New("chat: model is required")
	}
	endpoint := api.Endpoint{
		BaseURL: role.BaseURL,
		Model:   role.Model,
		Auth:    api.BearerAuth(role.APIKey),
	}
	switch role.Protocol {
	case config.ProtocolChatCompletions:
		return &completionsChat{client: openaichat.New(endpoint), model: role.Model}, nil
	default:
		return nil, fmt.Errorf("chat: unknown protocol %q", role.Protocol)
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
