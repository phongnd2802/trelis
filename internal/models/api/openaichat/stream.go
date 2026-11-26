package openaichat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
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
type toolCallAssembler struct {
	byIndex map[int]*types.ToolCall
}

func (a *toolCallAssembler) add(deltas []wireToolCall) {
	for i, d := range deltas {
		idx := i
		if d.Index != nil {
			idx = *d.Index
		}
		if a.byIndex == nil {
			a.byIndex = map[int]*types.ToolCall{}
		}
		tc, ok := a.byIndex[idx]
		if !ok {
			tc = &types.ToolCall{}
			a.byIndex[idx] = tc
		}
		if d.ID != "" {
			tc.ID = d.ID
		}
		if d.Function.Name != "" {
			tc.Name = d.Function.Name
		}
		tc.Arguments += d.Function.Arguments
	}
}

func (a *toolCallAssembler) result() []types.ToolCall {
	if len(a.byIndex) == 0 {
		return nil
	}
	idxs := make([]int, 0, len(a.byIndex))
	for i := range a.byIndex {
		idxs = append(idxs, i)
	}
	slices.Sort(idxs)
	out := make([]types.ToolCall, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, *a.byIndex[i])
	}
	return out
}
