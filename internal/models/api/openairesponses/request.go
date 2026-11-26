// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/openairesponses/request.go.
// Trelis drops multimodal input, reasoning items and summaries, prompt
// caching, runtime notices, JSON-format hints and extra_body; roles come
// from config, not a model database.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

// Package openairesponses implements the OpenAI Responses wire protocol
// (POST /responses). Every vendor-specific deviation is driven by
// api.OpenAIResponsesSettings; this package contains no vendor names.
package openairesponses

import (
	"strings"

	"github.com/phongnd2802/trelis/internal/models/api"
)

// Config is everything the client needs, already resolved by the caller.
type Config struct {
	Endpoint api.Endpoint
	Settings api.OpenAIResponsesSettings
}

// --- input items ---

type inputMessage struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// inputText is one text part of an assistant message carried back into the
// next turn.
type inputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionCallOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// minMaxOutputTokens is OpenAI's floor for max_output_tokens. Chat Completions
// accepts any positive budget, so a small one that works today would start
// failing the moment a role switches protocol.
const minMaxOutputTokens = 16

// convertMessages splits neutral messages into the instructions string and
// the ordered input item list. The Responses protocol has no system (or
// developer) message in its input: such a preamble travels as `instructions`
// instead.
func convertMessages(messages []api.Message) (string, []any) {
	var instructions []string
	items := make([]any, 0, len(messages))
	for _, msg := range messages {
		msg = api.NeutralizeMessageSpecialTokens(msg)
		switch msg.Role {
		case "system", "developer":
			if msg.Content != "" {
				instructions = append(instructions, msg.Content)
			}
		case "assistant":
			items = append(items, assistantItems(msg)...)
		case "tool":
			items = append(items, functionCallOutputItem{
				Type:   "function_call_output",
				CallID: msg.ToolCallID,
				Output: msg.Content,
			})
		default:
			// Any other role is a free-form turn in this request model; the
			// Responses input only has user messages for that.
			items = append(items, inputMessage{Type: "message", Role: "user", Content: msg.Content})
		}
	}
	return strings.Join(instructions, "\n\n"), items
}

// assistantItems replays a prior assistant turn: its text, then one
// function_call per tool call. A turn that only called tools carries no
// message item, the way the Responses API itself represents it.
func assistantItems(msg api.Message) []any {
	var items []any
	if msg.Content != "" || len(msg.ToolCalls) == 0 {
		items = append(items, inputMessage{
			Type:    "message",
			Role:    "assistant",
			Content: []inputText{{Type: "output_text", Text: msg.Content}},
		})
	}
	for _, tc := range msg.ToolCalls {
		items = append(items, functionCallItem{
			Type:      "function_call",
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return items
}

// buildBody assembles the request body. It returns a map so the shape stays
// easy to compare in golden tests; key order in the encoded JSON is
// alphabetical, which keeps those tests stable.
func (c *Client) buildBody(messages []api.Message, opts *api.Options, stream bool) map[string]any {
	s := c.cfg.Settings
	instructions, input := convertMessages(messages)
	body := map[string]any{
		"model": c.cfg.Endpoint.Model,
		"input": input,
	}
	if instructions != "" {
		body["instructions"] = instructions
	}
	if stream {
		body["stream"] = true
	}
	// Nothing is kept server-side: a stateless service must be able to replay
	// its own turns, which it does through the input items.
	if s.SupportsStore {
		body["store"] = false
	}
	if opts != nil {
		c.applySampling(body, opts)
		if budget := opts.CompletionBudget(); budget > 0 && s.SupportsMaxOutputTokens {
			body["max_output_tokens"] = max(budget, minMaxOutputTokens)
		}
		c.applyTools(body, opts)
	}
	return body
}

func (c *Client) applySampling(body map[string]any, opts *api.Options) {
	if !c.cfg.Settings.SupportsTemperature {
		return
	}
	if opts.Temperature > 0 {
		body["temperature"] = opts.Temperature
	}
	if opts.TopP > 0 {
		body["top_p"] = opts.TopP
	}
}

// applyTools emits the flat Responses tool shape: unlike Chat Completions,
// name, description and parameters sit on the tool itself.
func (c *Client) applyTools(body map[string]any, opts *api.Options) {
	if len(opts.Tools) == 0 {
		return
	}
	tools := make([]map[string]any, 0, len(opts.Tools))
	for _, tool := range opts.Tools {
		entry := map[string]any{
			"type":        orDefault(tool.Type, "function"),
			"name":        tool.Function.Name,
			"description": tool.Function.Description,
		}
		if len(tool.Function.Parameters) > 0 {
			entry["parameters"] = tool.Function.Parameters
		}
		tools = append(tools, entry)
	}
	body["tools"] = tools
	if opts.ParallelToolCalls != nil && c.cfg.Settings.SupportsParallelToolCalls {
		body["parallel_tool_calls"] = *opts.ParallelToolCalls
	}
	switch opts.ToolChoice {
	case "":
	case "none", "required", "auto":
		body["tool_choice"] = opts.ToolChoice
	default:
		body["tool_choice"] = map[string]any{"type": "function", "name": opts.ToolChoice}
	}
}

// BuildRequestBody is the golden-test entry point: it returns the exact JSON
// object that would be sent for the given inputs.
func (c *Client) BuildRequestBody(messages []api.Message, opts *api.Options, stream bool) map[string]any {
	return c.buildBody(messages, opts, stream)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
