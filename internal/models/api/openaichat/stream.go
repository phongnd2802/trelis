package openaichat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/phongnd2802/trelis/internal/types"
)

// ChatStream sends messages with stream on. A failed request is returned
// here; anything that goes wrong later arrives as Err on the final event.
// Usage is requested with stream_options.include_usage and is zero if the
// provider ignores it.
func (c *Client) ChatStream(ctx context.Context, messages []types.ChatMessage, opts types.ChatOptions) (<-chan types.StreamingResponse, error) {
	body, err := json.Marshal(c.request(messages, opts, true))
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	resp, err := c.endpoint.Post(ctx, "/chat/completions", body)
	if err != nil {
		return nil, err
	}

	out := make(chan types.StreamingResponse)
	go func() {
		defer close(out)
		defer func() { _ = resp.Body.Close() }()
		final := readStream(ctx, resp.Body, out)
		final.Done = true
		select {
		case out <- final:
		case <-ctx.Done():
		}
	}()
	return out, nil
}

// readStream forwards content deltas to out and returns the final event,
// without Done set.
func readStream(ctx context.Context, body io.Reader, out chan<- types.StreamingResponse) types.StreamingResponse {
	var (
		final types.StreamingResponse
		calls toolCallAssembler
		r     = bufio.NewReader(body)
	)
	for {
		line, err := r.ReadString('\n')
		if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				final.ToolCalls = calls.result()
				return final
			}
			var ch chunk
			if jerr := json.Unmarshal([]byte(data), &ch); jerr != nil {
				final.Err = fmt.Errorf("decode stream event: %w: %.200s", jerr, data)
				return final
			}
			if ch.Error != nil {
				final.Err = fmt.Errorf("provider error in stream: %s", ch.Error.Message)
				return final
			}
			if ch.Usage != nil {
				final.Usage = ch.Usage.toUsage()
			}
			for _, choice := range ch.Choices {
				if choice.FinishReason != "" {
					final.FinishReason = choice.FinishReason
				}
				calls.add(choice.Delta.ToolCalls)
				d := choice.Delta
				if d.Content == "" && d.ReasoningContent == "" {
					continue
				}
				select {
				case out <- types.StreamingResponse{Content: d.Content, ReasoningContent: d.ReasoningContent}:
				case <-ctx.Done():
					final.Err = ctx.Err()
					return final
				}
			}
		}
		if err != nil {
			// Some providers end the stream without [DONE]; a finish reason
			// means the answer is complete.
			if errors.Is(err, io.EOF) && final.FinishReason != "" {
				final.ToolCalls = calls.result()
				return final
			}
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			final.Err = fmt.Errorf("read stream: %w", err)
			return final
		}
	}
}

// toolCallAssembler joins streamed tool-call fragments: the first fragment
// of a call carries its id and name, later ones append to the arguments.
// Fragments are matched by index; a provider that omits index sends calls
// one after another, so there a new id starts a new call and a fragment
// without one continues the last call.
type toolCallAssembler struct {
	calls   []types.ToolCall
	byIndex map[int]int // stream index -> position in calls
}

func (a *toolCallAssembler) add(deltas []wireToolCall) {
	for _, d := range deltas {
		pos := a.position(d)
		tc := &a.calls[pos]
		if d.ID != "" {
			tc.ID = d.ID
		}
		if d.Function.Name != "" {
			tc.Name = d.Function.Name
		}
		tc.Arguments += d.Function.Arguments
	}
}

// position finds the call d continues, starting a new one when needed.
func (a *toolCallAssembler) position(d wireToolCall) int {
	if d.Index != nil {
		if pos, ok := a.byIndex[*d.Index]; ok {
			return pos
		}
		if a.byIndex == nil {
			a.byIndex = map[int]int{}
		}
		a.byIndex[*d.Index] = len(a.calls)
	} else if n := len(a.calls); n > 0 && (d.ID == "" || a.calls[n-1].ID == d.ID) {
		return n - 1
	}
	a.calls = append(a.calls, types.ToolCall{})
	return len(a.calls) - 1
}

func (a *toolCallAssembler) result() []types.ToolCall {
	return a.calls
}
