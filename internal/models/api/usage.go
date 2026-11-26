// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/usage.go.
// Trelis keeps the token counters and drops the prompt-cache accounting.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

package api

import "github.com/phongnd2802/trelis/internal/types"

// OpenAIUsage is the usage block OpenAI-compatible endpoints return.
type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ToTokenUsage normalizes the counters; a vendor that omits total_tokens
// gets prompt plus completion.
func (u OpenAIUsage) ToTokenUsage() types.TokenUsage {
	out := types.TokenUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}
