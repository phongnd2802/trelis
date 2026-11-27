// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/types/chat.go.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

package types

// TokenUsage holds token consumption statistics returned by the model API.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// LLMToolCall represents a function/tool call from the LLM
type LLMToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function FunctionCall `json:"function"`
}

// FunctionCall represents the function details
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// ChatResponse chat response
type ChatResponse struct {
	Content string `json:"content"`
	// ReasoningContent is the reasoning a thinking model (DeepSeek, vLLM
	// reasoning parsers, ...) produced this round.
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []LLMToolCall `json:"tool_calls,omitempty"`
	FinishReason     string        `json:"finish_reason,omitempty"`
	Usage            TokenUsage    `json:"usage"`
}

// ResponseType is the kind of one StreamResponse.
type ResponseType string

const (
	// ResponseTypeAnswer carries answer text. The closing chunk of a healthy
	// stream is an answer chunk with Done set; a broken stream closes with an
	// error chunk instead.
	ResponseTypeAnswer ResponseType = "answer"
	// ResponseTypeThinking carries reasoning text; one Done marker closes it
	// before the first answer token. That marker is not a stream close.
	ResponseTypeThinking ResponseType = "thinking"
	// ResponseTypeToolCall announces a tool call as soon as its name is known.
	ResponseTypeToolCall ResponseType = "tool_call"
	// ResponseTypeError closes a broken stream; Content is the error.
	ResponseTypeError ResponseType = "error"
)

// StreamResponse is one chunk of a streaming chat call. A stream has exactly
// one closing chunk: the answer chunk with Done set, or the error chunk when
// the stream broke. It carries the assembled ToolCalls, the Usage and the
// FinishReason. The thinking-done marker is the only other chunk with Done
// set, and it does not close the stream; content chunks never set Done.
type StreamResponse struct {
	ID           string                 `json:"id"`
	ResponseType ResponseType           `json:"response_type"`
	Content      string                 `json:"content"`
	Done         bool                   `json:"done"`
	ToolCalls    []LLMToolCall          `json:"tool_calls,omitempty"`
	Data         map[string]interface{} `json:"data,omitempty"`
	Usage        *TokenUsage            `json:"usage,omitempty"`
	FinishReason string                 `json:"finish_reason,omitempty"`
}

// FinishReasonIncomplete marks a stream that broke before the provider sent a
// finish reason.
const FinishReasonIncomplete = "incomplete"
