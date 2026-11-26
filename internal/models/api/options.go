// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/options.go.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

package api

// Options are the chat options. They are protocol-neutral: every field is a
// capability the caller asks for, and the protocol package decides how (or
// whether) the vendor can honour it.
type Options struct {
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p"`
	// MaxTokens and MaxCompletionTokens are aliases for one completion budget.
	// Callers may set either; CompletionBudget() prefers MaxCompletionTokens.
	// The outbound JSON carries exactly one field, named per vendor.
	MaxTokens           int    `json:"max_tokens"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
	Tools               []Tool `json:"tools,omitempty"`
	// ToolChoice is "auto", "required", "none" or a specific tool name.
	ToolChoice string `json:"tool_choice,omitempty"`
	// ParallelToolCalls nil leaves it to the model.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

// CompletionBudget is the single per-call generation cap. When both aliases
// are set, the newer MaxCompletionTokens wins.
func (o *Options) CompletionBudget() int {
	if o == nil {
		return 0
	}
	if o.MaxCompletionTokens > 0 {
		return o.MaxCompletionTokens
	}
	return o.MaxTokens
}
