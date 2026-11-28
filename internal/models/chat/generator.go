package chat

import (
	"context"

	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// generator adapts a Chat role to the narrow Generator contract the
// application layer takes.
type generator struct {
	chat Chat
}

// Generator returns c as the chat model role the Pipeline Answer service
// calls.
func Generator(c Chat) interfaces.Generator {
	return &generator{chat: c}
}

// Generate sends one non-streaming chat call with the turns in order.
func (g *generator) Generate(ctx context.Context, turns []types.ChatTurn) (*types.ChatResponse, error) {
	return g.chat.Chat(ctx, generatorMessages(turns), nil)
}

// GenerateStream sends one streaming chat call with the turns in order. The
// caller drains the channel until it closes, as ChatStream documents.
func (g *generator) GenerateStream(ctx context.Context, turns []types.ChatTurn) (<-chan types.StreamResponse, error) {
	return g.chat.ChatStream(ctx, generatorMessages(turns), nil)
}

// generatorMessages maps the application layer's turns to chat messages.
func generatorMessages(turns []types.ChatTurn) []Message {
	messages := make([]Message, len(turns))
	for i, turn := range turns {
		messages[i] = Message{Role: turn.Role, Content: turn.Content}
	}
	return messages
}

// GetModelName returns the model name sent on the wire.
func (g *generator) GetModelName() string { return g.chat.GetModelName() }
