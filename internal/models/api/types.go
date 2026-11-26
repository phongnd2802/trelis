// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/types.go.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

// Package api holds the protocol-neutral request model shared by every wire
// protocol implementation (OpenAI Chat Completions now, OpenAI Responses with
// #19) plus the helpers those implementations share: the endpoint, SSE
// reading, streaming tool-call assembly and usage decoding.
//
// Protocol packages live under api/<protocol>.
package api

import "encoding/json"

// API identifies a wire protocol. It is the value of a chat role's
// `protocol` key and decides which protocol package talks to the endpoint.
type API string

const (
	// APIOpenAICompletions is the OpenAI Chat Completions protocol, the
	// lingua franca of almost every third-party vendor.
	APIOpenAICompletions API = "openai-completions"
)

// Known reports whether the API value names a protocol this build ships.
// APIOpenAIResponses joins with #19.
func (a API) Known() bool {
	switch a {
	case APIOpenAICompletions:
		return true
	}
	return false
}

// Tool represents a function/tool definition
type Tool struct {
	Type     string      `json:"type"` // "function"
	Function FunctionDef `json:"function"`
}

// FunctionDef represents a function definition
type FunctionDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Parameters is the JSON Schema of the arguments.
	Parameters json.RawMessage `json:"parameters"`
}

// Message is one chat message in the protocol-neutral request model.
type Message struct {
	Role       string     `json:"role"`                   // system, user, assistant, tool
	Content    string     `json:"content"`                // message text
	Name       string     `json:"name,omitempty"`         // Function/tool name (for tool role)
	ToolCallID string     `json:"tool_call_id,omitempty"` // Tool call ID (for tool role)
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // Tool calls (for assistant role)
	// ReasoningContent is the reasoning a thinking model produced on a prior
	// assistant turn. Some vendors (DeepSeek, Kimi) reject a multi-turn
	// request that drops it; the others ignore the field.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ToolCall represents a tool call in a message
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function FunctionCall `json:"function"`
}

// FunctionCall represents a function call
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// ReasoningEffort is the protocol-neutral thinking level.
type ReasoningEffort string

const (
	// ReasoningOff disables extended thinking where the model allows it.
	ReasoningOff ReasoningEffort = "off"
	// ReasoningAuto enables thinking but leaves the intensity to the provider
	// default. This is what the legacy boolean `thinking: true` maps to.
	ReasoningAuto ReasoningEffort = "auto"
	// ReasoningMinimal is the weakest graded level.
	ReasoningMinimal ReasoningEffort = "minimal"
	// ReasoningLow is the low graded level.
	ReasoningLow ReasoningEffort = "low"
	// ReasoningMedium is the medium graded level.
	ReasoningMedium ReasoningEffort = "medium"
	// ReasoningHigh is the high graded level.
	ReasoningHigh ReasoningEffort = "high"
	// ReasoningXHigh is the extra-high graded level (OpenAI, Zhipu).
	ReasoningXHigh ReasoningEffort = "xhigh"
	// ReasoningMax is the strongest graded level.
	ReasoningMax ReasoningEffort = "max"
)

// ThinkingLevelMap maps protocol-neutral levels to a vendor's own vocabulary.
type ThinkingLevelMap map[ReasoningEffort]*string
