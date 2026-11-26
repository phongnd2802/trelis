// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/openaicompletions/request.go.
// Trelis drops multimodal parts, thinking-level dialects, prompt caching,
// extra_body and vendor tool-call fields.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

// Package openaicompletions implements the OpenAI Chat Completions wire
// protocol. Every vendor-specific deviation is driven by
// api.OpenAICompletionsSettings; this package contains no vendor names.
package openaicompletions

import (
	"github.com/phongnd2802/trelis/internal/models/api"
)

// Config is everything the client needs, already resolved by the caller.
type Config struct {
	Endpoint api.Endpoint
	Settings api.OpenAICompletionsSettings
}

type wireToolCall struct {
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type"`
	Function wireFunctionCall `json:"function"`
}

type wireFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"`
	Name             string         `json:"name,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
}

// convertMessages maps neutral messages onto Chat Completions messages.
func (c *Client) convertMessages(messages []api.Message) []wireMessage {
	s := c.cfg.Settings
	out := make([]wireMessage, 0, len(messages))
	for _, msg := range messages {
		wm := wireMessage{Role: msg.Role}

		if msg.Content != "" || len(msg.ToolCalls) == 0 {
			wm.Content = msg.Content
		} else {
			// Assistant tool-call turn with empty content: emit an explicit
			// `"content": ""` instead of omitting the field. Strict local
			// engines (vLLM / Xinference serving Qwen-family Jinja chat
			// templates) treat a missing content field as undefined and crash
			// when rendering the assistant turn for the next round. The
			// *string keeps `omitempty` from dropping the field: a non-nil
			// interface holding a pointer serializes as "".
			empty := ""
			wm.Content = &empty
		}

		if len(msg.ToolCalls) > 0 {
			wm.ToolCalls = make([]wireToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
					ID:       tc.ID,
					Type:     orDefault(tc.Type, "function"),
					Function: wireFunctionCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
				})
			}
		}

		if msg.Role == "tool" {
			wm.ToolCallID = msg.ToolCallID
			wm.Name = msg.Name
		}

		if msg.Role == "assistant" && s.ReplayReasoningContent && msg.ReasoningContent != "" {
			rc := msg.ReasoningContent
			wm.ReasoningContent = &rc
		}
		out = append(out, wm)
	}
	return out
}

// buildBody assembles the request body. It returns a map so vendor-specific
// top-level fields can be added without a struct per vendor; key order in
// the encoded JSON is alphabetical, which keeps golden tests stable.
func (c *Client) buildBody(messages []api.Message, opts *api.Options, stream bool) map[string]any {
	s := c.cfg.Settings
	body := map[string]any{
		"model":    c.cfg.Endpoint.Model,
		"messages": c.convertMessages(messages),
	}
	if stream {
		body["stream"] = true
		if s.SupportsUsageInStreaming {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	if opts != nil {
		c.applySampling(body, opts)
		if budget := opts.CompletionBudget(); budget > 0 {
			body[orDefault(s.MaxTokensField, "max_completion_tokens")] = budget
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

func (c *Client) applyTools(body map[string]any, opts *api.Options) {
	s := c.cfg.Settings
	if len(opts.Tools) == 0 {
		return
	}
	tools := make([]map[string]any, 0, len(opts.Tools))
	for _, tool := range opts.Tools {
		fn := map[string]any{
			"name":        tool.Function.Name,
			"description": tool.Function.Description,
		}
		if len(tool.Function.Parameters) > 0 {
			fn["parameters"] = tool.Function.Parameters
		}
		tools = append(tools, map[string]any{"type": orDefault(tool.Type, "function"), "function": fn})
	}
	body["tools"] = tools
	if opts.ParallelToolCalls != nil && s.SupportsParallelToolCalls {
		body["parallel_tool_calls"] = *opts.ParallelToolCalls
	}
	if opts.ToolChoice == "" {
		return
	}
	switch opts.ToolChoice {
	case "none", "required", "auto":
		if s.AllowsToolChoice(opts.ToolChoice) {
			body["tool_choice"] = opts.ToolChoice
		}
	default:
		if s.AllowsToolChoice("function") {
			body["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": opts.ToolChoice},
			}
		}
	}
}

// BuildRequestBody is the golden-test entry point: it returns the exact
// JSON object that would be sent for the given inputs.
func (c *Client) BuildRequestBody(messages []api.Message, opts *api.Options, stream bool) map[string]any {
	return c.buildBody(messages, opts, stream)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
