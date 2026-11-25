package api

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
