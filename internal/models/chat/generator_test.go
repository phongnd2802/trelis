package chat

import (
	"context"
	"reflect"
	"testing"

	"github.com/phongnd2802/trelis/internal/types"
)

// recordingChat is a Chat that records the one call it receives and answers a
// fixed reply.
type recordingChat struct {
	model       string
	reply       *types.ChatResponse
	gotMessages []Message
	gotOptions  *ChatOptions
	calls       int
}

func (c *recordingChat) Chat(_ context.Context, messages []Message, opts *ChatOptions) (*types.ChatResponse, error) {
	c.calls++
	c.gotMessages = append([]Message(nil), messages...)
	c.gotOptions = opts
	return c.reply, nil
}

func (c *recordingChat) ChatStream(context.Context, []Message, *ChatOptions) (<-chan types.StreamResponse, error) {
	panic("ChatStream called; the generator sends one non-streaming call")
}

func (c *recordingChat) GetModelName() string { return c.model }

func TestGeneratorSendsTheTurnsAsMessagesInOrder(t *testing.T) {
	reply := &types.ChatResponse{
		Content:      "Điều 1 [1].",
		FinishReason: "stop",
		Usage:        types.TokenUsage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
	}
	fake := &recordingChat{model: "test-chat", reply: reply}
	turns := []types.ChatTurn{
		{Role: types.ChatRoleSystem, Content: "answer only from the sources"},
		{Role: types.ChatRoleUser, Content: "Sources:\n[1] Điều 1.\n\nQuestion: what?"},
	}

	got, err := Generator(fake).Generate(context.Background(), turns)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("chat role called %d times, want once", fake.calls)
	}
	want := []Message{
		{Role: types.ChatRoleSystem, Content: turns[0].Content},
		{Role: types.ChatRoleUser, Content: turns[1].Content},
	}
	if !reflect.DeepEqual(fake.gotMessages, want) {
		t.Errorf("messages = %+v, want the turns in order %+v", fake.gotMessages, want)
	}
	if fake.gotOptions != nil {
		t.Errorf("options = %+v, want nil", fake.gotOptions)
	}
	if got != reply {
		t.Errorf("reply = %+v, want the chat role's reply returned unchanged", got)
	}
	if got.Content != reply.Content || got.Usage != reply.Usage || got.FinishReason != reply.FinishReason {
		t.Errorf("reply = %+v, want content, usage and finish reason unchanged", got)
	}
}

func TestGeneratorReturnsTheModelName(t *testing.T) {
	if got := Generator(&recordingChat{model: "test-chat"}).GetModelName(); got != "test-chat" {
		t.Errorf("GetModelName = %q, want %q", got, "test-chat")
	}
}
