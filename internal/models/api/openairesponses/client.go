package openairesponses

import "github.com/phongnd2802/trelis/internal/models/api"

// defaultPath is where the Responses API lives under the base URL.
// Chat and streaming arrive with #19.
const defaultPath = "/responses"

type Client struct {
	cfg Config
}

func New(cfg Config) *Client {
	if cfg.ThinkingLevels == nil {
		cfg.ThinkingLevels = api.ThinkingLevelMap{}
	}

	return &Client{cfg: cfg}
}
