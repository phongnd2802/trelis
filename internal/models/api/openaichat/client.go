// Package openaichat is a client for the OpenAI-compatible Chat Completions
// protocol (POST {base_url}/chat/completions).
package openaichat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/types"
)

// Client talks to one Chat Completions endpoint.
type Client struct {
	endpoint api.Endpoint
}

func New(endpoint api.Endpoint) *Client {
	return &Client{endpoint: endpoint}
}

// Chat sends messages and waits for the whole answer.
func (c *Client) Chat(ctx context.Context, messages []types.ChatMessage, opts types.ChatOptions) (*types.ChatResponse, error) {
	body, err := json.Marshal(c.request(messages, opts, false))
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	resp, err := c.endpoint.Post(ctx, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var out completion
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("response has no choices: %.200s", raw)
	}
	choice := out.Choices[0]
	return &types.ChatResponse{
		Content:          choice.Message.Content,
		ReasoningContent: choice.Message.ReasoningContent,
		ToolCalls:        fromWireToolCalls(choice.Message.ToolCalls),
		FinishReason:     choice.FinishReason,
		Usage:            out.Usage.toUsage(),
	}, nil
}

func (c *Client) request(messages []types.ChatMessage, opts types.ChatOptions, stream bool) request {
	r := request{
		Model:       c.endpoint.Model,
		Messages:    toWireMessages(messages),
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Tools:       toWireTools(opts.Tools),
		ToolChoice:  toWireToolChoice(opts.ToolChoice),
	}
	if stream {
		r.Stream = true
		r.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return r
}
