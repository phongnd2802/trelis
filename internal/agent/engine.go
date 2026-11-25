package agent

import "github.com/phongnd2802/trelis/internal/models/chat"

type AgentEngine struct {
	chatModel chat.Chat
}

func NewAgentEngine(chatModel chat.Chat) *AgentEngine {
	engine := &AgentEngine{
		chatModel: chatModel,
	}

	return engine
}
