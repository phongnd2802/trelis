package chat

import (
	"context"

	"github.com/phongnd2802/trelis/internal/types"
)

var _ Chat = (*debugChat)(nil)

type debugChat struct {
	inner Chat
}

func (d *debugChat) GetModelName() string { return d.inner.GetModelName() }

func (d *debugChat) Chat(ctx context.Context, messages []Message, opts *ChatOptions) (*types.ChatResponse, error) {
	return d.inner.Chat(ctx, messages, opts)
}

func (d *debugChat) ChatStream(ctx context.Context, messages []Message, opts *ChatOptions) (<-chan types.StreamResponse, error) {
	return d.inner.ChatStream(ctx, messages, opts)
}
