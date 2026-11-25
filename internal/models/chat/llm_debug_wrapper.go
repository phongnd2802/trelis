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

func (d *debugChat) ModelID() string { return d.inner.ModelID() }

func (d *debugChat) Chat(ctx context.Context, messages []Message, opts *ChatOptions) (*types.ChatResponse, error) {
	resp, err := d.inner.Chat(ctx, messages, opts)
	return resp, err
}

func (d *debugChat) ChatStream(ctx context.Context, messages []Message, opts *ChatOptions) (<-chan types.StreamingResponse, error) {
	return d.inner.ChatStream(ctx, messages, opts)
}
