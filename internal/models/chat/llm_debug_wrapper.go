package chat

import (
	"context"

	"github.com/phongnd2802/trelis/internal/types"
)

var _ Chat = (*debugChat)(nil)

type debugChat struct {
	inner Chat
}

func (d *debugChat) ModelName() string { return d.inner.ModelName() }

func (d *debugChat) Chat(ctx context.Context, messages []types.ChatMessage, opts *types.ChatOptions) (*types.ChatResponse, error) {
	return d.inner.Chat(ctx, messages, opts)
}

func (d *debugChat) ChatStream(ctx context.Context, messages []types.ChatMessage, opts *types.ChatOptions) (<-chan types.StreamingResponse, error) {
	return d.inner.ChatStream(ctx, messages, opts)
}
