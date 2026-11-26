// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/stream_assembler.go
// and internal/models/api/thinking_emitter.go. Trelis drops the sandbox
// file-progress, thinking-tool extraction and provider-metadata branches.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

package api

import (
	"context"
	"log/slog"
	"sort"

	"github.com/phongnd2802/trelis/internal/types"
)

// ToolCallDelta is one streamed fragment of a tool call, already lifted out
// of whatever envelope the protocol uses (OpenAI delta.tool_calls, Responses
// function_call_arguments.delta, ...).
type ToolCallDelta struct {
	Index     int
	ID        string
	Type      string
	Name      string
	Arguments string
}

// Delta is one protocol-neutral streaming event. A single wire chunk may
// carry several of these fields at once.
type Delta struct {
	Reasoning    string
	Content      string
	ToolCalls    []ToolCallDelta
	FinishReason string
}

// StreamAssembler turns protocol deltas into the types.StreamResponse
// sequence the answering code consumes. It owns the "reasoning then answer"
// hand-off, tool-call fragment assembly and the early tool-call notification
// (ResponseTypeToolCall) once a function name has stabilised.
type StreamAssembler struct {
	ctx              context.Context
	model            string
	toolCallMap      map[int]*types.LLMToolCall
	lastFunctionName map[int]string
	nameNotified     map[int]bool
	// Usage is captured from the final stream chunk when the vendor reports it.
	Usage *types.TokenUsage
	// LastFinishReason is the last observed finish_reason for the EOF fallback.
	LastFinishReason string

	// thinking is set while a thinking-done marker is owed.
	thinking bool
	aborted  bool
}

// NewStreamAssembler creates an assembler for one stream.
func NewStreamAssembler(ctx context.Context, model string) *StreamAssembler {
	return &StreamAssembler{
		ctx:              ctx,
		model:            model,
		toolCallMap:      make(map[int]*types.LLMToolCall),
		lastFunctionName: make(map[int]string),
		nameNotified:     make(map[int]bool),
	}
}

// emit hands one chunk to the consumer, giving up as soon as the call context
// is done. A bare `ch <-` would block forever when the consumer abandons the
// channel, pinning the reader goroutine and the response body. Once a send is
// abandoned the assembler stays aborted so the remaining chunks are dropped.
func (a *StreamAssembler) emit(ch chan<- types.StreamResponse, resp types.StreamResponse) bool {
	if a.aborted {
		return false
	}
	// Prefer the cancellation branch deterministically: with both cases ready
	// select would pick at random and could push into an abandoned channel.
	select {
	case <-a.ctx.Done():
		a.aborted = true
		return false
	default:
	}
	select {
	case ch <- resp:
		return true
	case <-a.ctx.Done():
		a.aborted = true
		return false
	}
}

// Aborted reports whether the consumer went away mid-stream. Protocol loops
// check it to stop reading instead of decoding a stream nobody listens to.
func (a *StreamAssembler) Aborted() bool { return a.aborted }

// OrderedToolCalls returns the assembled calls in index order, or nil.
//
// The indices are the vendor's, not ours: gateways number calls from 1, and
// parallel calls can arrive with gaps, so the keys are sorted instead of
// walking 0..len(map).
func (a *StreamAssembler) OrderedToolCalls() []types.LLMToolCall {
	if len(a.toolCallMap) == 0 {
		return nil
	}
	indices := make([]int, 0, len(a.toolCallMap))
	for idx := range a.toolCallMap {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	result := make([]types.LLMToolCall, 0, len(a.toolCallMap))
	for _, idx := range indices {
		if tc := a.toolCallMap[idx]; tc != nil {
			result = append(result, *tc)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// ToolCallCount reports how many distinct tool calls have been seen.
func (a *StreamAssembler) ToolCallCount() int { return len(a.toolCallMap) }

// SetUsage records usage from the vendor's final chunk.
func (a *StreamAssembler) SetUsage(u types.TokenUsage) { a.Usage = &u }

// Process consumes one delta and emits the resulting stream responses.
func (a *StreamAssembler) Process(ch chan<- types.StreamResponse, d Delta) {
	isDone := d.FinishReason != ""
	if isDone {
		a.LastFinishReason = d.FinishReason
	}

	if len(d.ToolCalls) > 0 {
		a.processToolCalls(ch, d.ToolCalls)
	}

	if d.Reasoning != "" {
		a.Emit(ch, d.Reasoning)
	}

	if d.Content != "" {
		a.Finish(ch)
		a.emit(ch, types.StreamResponse{
			ResponseType: types.ResponseTypeAnswer,
			Content:      d.Content,
			Done:         isDone,
			ToolCalls:    a.OrderedToolCalls(),
			FinishReason: d.FinishReason,
		})
	}

	if isDone && len(a.toolCallMap) > 0 {
		a.emit(ch, types.StreamResponse{
			ResponseType: types.ResponseTypeAnswer,
			Done:         true,
			ToolCalls:    a.OrderedToolCalls(),
			FinishReason: d.FinishReason,
		})
	}

	if isDone {
		a.Finish(ch)
	}

	if isDone && d.Content == "" && len(a.toolCallMap) == 0 {
		a.emit(ch, types.StreamResponse{
			ResponseType: types.ResponseTypeAnswer,
			Done:         true,
			FinishReason: d.FinishReason,
		})
	}
}

// Emit forwards a reasoning chunk and records that a thinking-done marker is
// still owed.
func (a *StreamAssembler) Emit(ch chan<- types.StreamResponse, content string) {
	a.thinking = true
	a.emit(ch, types.StreamResponse{ResponseType: types.ResponseTypeThinking, Content: content})
}

// Finish emits the single thinking-done marker if one is owed. Safe to call
// multiple times; only the first call after an Emit sends anything.
func (a *StreamAssembler) Finish(ch chan<- types.StreamResponse) {
	if !a.thinking {
		return
	}
	a.thinking = false
	a.emit(ch, types.StreamResponse{ResponseType: types.ResponseTypeThinking, Done: true})
}

// End emits the closing chunk once the vendor stream is exhausted. It carries
// the assembled tool calls and the usage.
func (a *StreamAssembler) End(ch chan<- types.StreamResponse) {
	a.Finish(ch)
	a.emit(ch, types.StreamResponse{
		ResponseType: types.ResponseTypeAnswer,
		Done:         true,
		ToolCalls:    a.OrderedToolCalls(),
		Usage:        a.Usage,
		FinishReason: a.LastFinishReason,
	})
}

// EndAtEOF closes a stream whose body simply ran out. A vendor that finished
// the message has reported a finish reason by then; without one the
// connection was most likely cut by a proxy, and the answer is marked
// incomplete instead of reaching the caller as a clean stop.
func (a *StreamAssembler) EndAtEOF(ch chan<- types.StreamResponse) {
	if a.LastFinishReason == "" {
		slog.WarnContext(a.ctx, "stream ended without a finish reason; marking the response incomplete", "model", a.model)
		a.LastFinishReason = types.FinishReasonIncomplete
	}
	a.End(ch)
}

// Fail emits the error chunk for a broken stream.
func (a *StreamAssembler) Fail(ch chan<- types.StreamResponse, err error) {
	slog.ErrorContext(a.ctx, "stream read error", "model", a.model, "err", err, "tool_calls_assembled", len(a.toolCallMap))
	a.Finish(ch)
	a.emit(ch, types.StreamResponse{
		ResponseType: types.ResponseTypeError,
		Content:      err.Error(),
		Done:         true,
		ToolCalls:    a.OrderedToolCalls(),
		Usage:        a.Usage,
		FinishReason: types.FinishReasonIncomplete,
	})
}

func (a *StreamAssembler) processToolCalls(ch chan<- types.StreamResponse, deltas []ToolCallDelta) {
	for _, tc := range deltas {
		index := tc.Index
		entry, exists := a.toolCallMap[index]
		if !exists || entry == nil {
			entry = &types.LLMToolCall{Type: tc.Type}
			a.toolCallMap[index] = entry
		}
		if tc.ID != "" {
			entry.ID = tc.ID
		}
		if tc.Type != "" {
			entry.Type = tc.Type
		}
		if entry.Type == "" {
			entry.Type = "function"
		}
		if tc.Name != "" {
			// Some runtimes (vLLM Ascend) resend the full name on every chunk;
			// treat an identical name as a repeat rather than a suffix.
			if entry.Function.Name != tc.Name {
				entry.Function.Name += tc.Name
			}
		}

		argsUpdated := false
		if tc.Arguments != "" {
			entry.Function.Arguments += tc.Arguments
			argsUpdated = true
		}

		// Announce the call once its name has stopped changing and its
		// arguments have started, so the UI can show it before it completes.
		currName := entry.Function.Name
		if currName != "" &&
			currName == a.lastFunctionName[index] &&
			argsUpdated &&
			!a.nameNotified[index] &&
			entry.ID != "" {
			a.emit(ch, types.StreamResponse{
				ResponseType: types.ResponseTypeToolCall,
				Data: map[string]interface{}{
					"tool_name":    currName,
					"tool_call_id": entry.ID,
				},
			})
			a.nameNotified[index] = true
		}
		a.lastFunctionName[index] = currName
	}
}
