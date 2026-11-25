package openairesponses

import "github.com/phongnd2802/trelis/internal/models/api"

type Config struct {
	Endpoint       api.Endpoint
	Settings       api.OpenAIResponsesSettings
	ThinkingLevels api.ThinkingLevelMap
	Reasoning      bool
}
