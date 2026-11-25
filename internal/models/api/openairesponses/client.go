package openairesponses

import "github.com/phongnd2802/trelis/internal/models/api"

// Client talks to an OpenAI Responses endpoint. Chat and streaming arrive with #19.
type Client struct {
	cfg Config
}

func New(cfg Config) *Client {
	if cfg.ThinkingLevels == nil {
		cfg.ThinkingLevels = api.ThinkingLevelMap{}
	}

	return &Client{cfg: cfg}
}
