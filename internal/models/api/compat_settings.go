// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/compat_settings.go.
// Trelis keeps only the settings its protocol clients read; there is no
// vendor catalog, so every role runs on the protocol defaults.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

package api

// OpenAICompletionsSettings drive every vendor deviation of the Chat
// Completions protocol, so the protocol package contains no vendor names.
type OpenAICompletionsSettings struct {
	// MaxTokensField is "max_tokens" or "max_completion_tokens".
	MaxTokensField           string
	SupportsUsageInStreaming bool
	// SupportsTemperature false drops every sampling parameter (o-series,
	// GPT-5 family).
	SupportsTemperature       bool
	ToolChoiceModes           []string
	SupportsParallelToolCalls bool
	// ReplayReasoningContent sends reasoning_content on prior assistant turns.
	ReplayReasoningContent bool
	// ReasoningFields are the delta keys that carry thinking text, in
	// priority order.
	ReasoningFields []string
}

// DefaultOpenAICompletions is the protocol baseline: plain OpenAI semantics.
func DefaultOpenAICompletions() OpenAICompletionsSettings {
	return OpenAICompletionsSettings{
		MaxTokensField:            "max_completion_tokens",
		SupportsUsageInStreaming:  true,
		SupportsTemperature:       true,
		ToolChoiceModes:           []string{"none", "auto", "required", "function"},
		SupportsParallelToolCalls: true,
		ReplayReasoningContent:    true,
		ReasoningFields:           []string{"reasoning_content", "reasoning", "reasoning_text"},
	}
}

// AllowsToolChoice reports whether a tool_choice mode is accepted.
func (s OpenAICompletionsSettings) AllowsToolChoice(mode string) bool {
	for _, m := range s.ToolChoiceModes {
		if m == mode {
			return true
		}
	}
	return false
}

// OpenAIResponsesSettings is the resolved Responses configuration.
type OpenAIResponsesSettings struct {
	SupportsDeveloperRole      bool
	SupportsMaxOutputTokens    bool
	SupportsReasoningSummary   bool
	SupportsEncryptedReasoning bool
	SupportsStore              bool
	SupportsTemperature        bool
	PromptCacheKey             bool
	SupportsLongCacheRetention bool
	SupportsParallelToolCalls  bool
	ExtraBody                  map[string]any
}
