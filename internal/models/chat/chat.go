// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/chat/chat.go.
// Trelis builds a chat role from its config.ChatRole instead of a model
// catalog and database row.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

// Package chat exposes the Chat interface used by the application layer and
// builds the right protocol client for a configured chat role (chat,
// extraction, judge). Every wire protocol lives under internal/models/api;
// this package only glues a role's config to its protocol client.
package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/phongnd2802/trelis/internal/config"
	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/models/api/openaicompletions"
	"github.com/phongnd2802/trelis/internal/types"
)

// Message and the other aliases below re-export the request model defined
// in internal/models/api so callers only import chat.
type (
	Message      = api.Message
	ToolCall     = api.ToolCall
	FunctionCall = api.FunctionCall
	Tool         = api.Tool
	FunctionDef  = api.FunctionDef
	ChatOptions  = api.Options
)

// Chat is one chat model role.
type Chat interface {
	// Chat performs a non-streaming chat.
	Chat(ctx context.Context, messages []Message, opts *ChatOptions) (*types.ChatResponse, error)

	// ChatStream performs a streaming chat. Drain the channel until it
	// closes: answer chunks may have Done set as soon as the provider sends
	// a finish reason, but the last chunk is the one that carries the tool
	// calls and the usage. A broken stream ends with a ResponseTypeError
	// chunk. Stopping early leaves the reader goroutine parked until ctx is
	// cancelled, so cancel ctx when abandoning a stream.
	ChatStream(ctx context.Context, messages []Message, opts *ChatOptions) (<-chan types.StreamResponse, error)

	// GetModelName returns the model name sent on the wire.
	GetModelName() string
}

// NewChat builds the protocol client for one configured chat role
// (models.chat, models.extraction or models.judge).
func NewChat(role config.ChatRole) (Chat, error) {
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
	case api.APIOpenAICompletions:
		return openaicompletions.New(openaicompletions.Config{
			Endpoint: endpoint,
			Settings: api.DefaultOpenAICompletions(),
		}), nil
	default:
		return nil, fmt.Errorf("unsupported chat api %q for model %s", role.Protocol, role.Model)
	}
}
