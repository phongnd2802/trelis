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
	"fmt"
	"strings"
	"time"

	"github.com/phongnd2802/trelis/internal/config"
	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/models/api/openaicompletions"
	"github.com/phongnd2802/trelis/internal/models/api/openairesponses"
	"github.com/phongnd2802/trelis/internal/models/providererror"
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
	// closes: exactly one closing chunk (an answer, or an error when the
	// stream broke) has Done set and carries the tool calls, the usage and
	// the finish reason. The thinking-done marker also sets Done but does not
	// close the stream. Stopping early leaves the reader goroutine parked
	// until ctx is cancelled, so cancel ctx when abandoning a stream.
	ChatStream(ctx context.Context, messages []Message, opts *ChatOptions) (<-chan types.StreamResponse, error)

	// GetModelName returns the model name sent on the wire.
	GetModelName() string
}

// NewChat builds the protocol client for one configured chat role
// (models.chat, models.extraction or models.judge). roleKey is that role's
// config key; the bare role name is what the model-call log and provider
// errors use.
func NewChat(roleKey string, role config.ChatRole) (Chat, error) {
	endpoint, err := role.APIEndpoint("chat")
	if err != nil {
		return nil, err
	}

	var inner Chat
	switch role.Protocol {
	case api.APIOpenAICompletions:
		inner = openaicompletions.New(openaicompletions.Config{
			Endpoint: endpoint,
			Settings: api.DefaultOpenAICompletions(),
		})
	case api.APIOpenAIResponses:
		inner = openairesponses.New(openairesponses.Config{
			Endpoint: endpoint,
			Settings: api.DefaultOpenAIResponses(),
		})
	default:
		return nil, fmt.Errorf("unsupported chat api %q for model %s", role.Protocol, role.Model)
	}
	return &loggingChat{role: strings.TrimPrefix(roleKey, "models."),
		protocol: role.Protocol, inner: inner}, nil
}

// loggingChat is a role's client with the one model-call line around each
// logical call. A chat send is not retried by the clients, so attempts is 1.
type loggingChat struct {
	// role names the role in the model-call line and a client-facing error:
	// the config key without its "models." prefix, so "models.chat" reads
	// as "chat".
	role     string
	protocol api.API
	inner    Chat
}

func (c *loggingChat) GetModelName() string { return c.inner.GetModelName() }

// call is the metadata every logging point shares.
func (c *loggingChat) call(start time.Time, failed bool, httpStatus int) logger.ModelCall {
	return logger.ModelCall{
		Role:       c.role,
		Model:      c.GetModelName(),
		Protocol:   string(c.protocol),
		Duration:   time.Since(start),
		Attempts:   1,
		Failed:     failed,
		HTTPStatus: httpStatus,
	}
}

func (c *loggingChat) Chat(ctx context.Context, messages []Message, opts *ChatOptions) (*types.ChatResponse, error) {
	start := time.Now()
	resp, err := c.inner.Chat(ctx, messages, opts)
	call := c.call(start, err != nil, api.HTTPStatusCode(err))
	if err == nil && resp != nil {
		call.HasUsage = resp.Usage.PromptTokens != 0 || resp.Usage.CompletionTokens != 0
		call.HasOutputTokens = true
		call.InputTokens = resp.Usage.PromptTokens
		call.OutputTokens = resp.Usage.CompletionTokens
	}
	logger.LogModelCall(ctx, call)
	if err != nil {
		return nil, providererror.Wrap(c.role, err)
	}
	return resp, nil
}

func (c *loggingChat) ChatStream(ctx context.Context, messages []Message, opts *ChatOptions) (<-chan types.StreamResponse, error) {
	start := time.Now()
	inner, err := c.inner.ChatStream(ctx, messages, opts)
	if err != nil {
		logger.LogModelCall(ctx, c.call(start, true, api.HTTPStatusCode(err)))
		return nil, providererror.Wrap(c.role, err)
	}

	out := make(chan types.StreamResponse)
	go func() {
		defer close(out)
		var (
			usage  *types.TokenUsage
			failed bool
		)
	stream:
		for resp := range inner {
			if resp.ResponseType == types.ResponseTypeError {
				failed = true
			}
			if resp.Done && resp.Usage != nil {
				usage = resp.Usage
			}
			select {
			case out <- resp:
			case <-ctx.Done():
				failed = true
				// Drain so the protocol reader sees its own ctx and closes
				// instead of staying parked on a channel nobody reads.
				for range inner {
				}
				break stream
			}
		}

		call := c.call(start, failed, 0)
		if usage != nil {
			call.HasUsage = true
			call.HasOutputTokens = true
			call.InputTokens = usage.PromptTokens
			call.OutputTokens = usage.CompletionTokens
		}
		logger.LogModelCall(ctx, call)
	}()
	return out, nil
}
