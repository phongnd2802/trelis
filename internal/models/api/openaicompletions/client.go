// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/openaicompletions/client.go.
// Trelis drops the multimodal retry, raw packet dumps, request/usage logging, DSML tool-call
// recovery, reasoning_details, vendor tool-call metadata and GetModelID (roles
// come from config, not a model database).
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

package openaicompletions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/types"
)

const defaultPath = "/chat/completions"

// Client talks Chat Completions to one endpoint.
type Client struct {
	cfg Config
}

// New creates a client. The endpoint must already carry auth.
func New(cfg Config) *Client {
	return &Client{cfg: cfg}
}

// GetModelName returns the wire model name.
func (c *Client) GetModelName() string { return c.cfg.Endpoint.Model }

func (c *Client) url() string { return c.cfg.Endpoint.Resolve(defaultPath) }

func (c *Client) send(ctx context.Context, messages []api.Message, opts *api.Options, stream bool) (*http.Response, error) {
	body := c.BuildRequestBody(messages, opts, stream)
	req, _, err := c.cfg.Endpoint.NewRequest(ctx, c.url(), body, stream)
	if err != nil {
		return nil, err
	}
	return c.cfg.Endpoint.Do(req)
}

// Chat performs a non-streaming completion.
func (c *Client) Chat(ctx context.Context, messages []api.Message, opts *api.Options) (*types.ChatResponse, error) {
	ctx, cancel := api.WithLLMTimeout(ctx, api.DefaultChatTimeout)
	defer cancel()

	resp, err := c.send(ctx, messages, opts, false)
	if err != nil {
		return nil, fmt.Errorf("create chat completion: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := api.ReadResponseCapped(resp.Body, 0)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return c.parseResponse(raw)
}

// ChatStream performs a streaming completion.
func (c *Client) ChatStream(
	ctx context.Context, messages []api.Message, opts *api.Options,
) (<-chan types.StreamResponse, error) {
	ctx, cancel := api.WithLLMTimeout(ctx, api.DefaultStreamTimeout)

	resp, err := c.send(ctx, messages, opts, true)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create chat completion stream: %w", err)
	}

	ch := make(chan types.StreamResponse)
	go func() {
		defer cancel()
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		c.processStream(ctx, resp.Body, ch)
	}()
	return ch, nil
}

// --- response decoding ---

type rawToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type rawMessage struct {
	Role             string            `json:"role,omitempty"`
	Content          messageContent    `json:"content,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	Reasoning        string            `json:"reasoning,omitempty"`
	ReasoningText    string            `json:"reasoning_text,omitempty"`
	ToolCalls        []json.RawMessage `json:"tool_calls,omitempty"`
}

// messageContent is the "content" of a message or delta. Most servers send a
// string, but the field may also be a list of typed parts, as Mistral does
// (string | ContentChunk[]): its reasoning models send "thinking" parts next to
// the "text" ones. Text parts are joined into Text and thinking parts into
// Thinking; other part types carry no answer text.
type messageContent struct {
	Text     string
	Thinking string
}

type contentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking json.RawMessage `json:"thinking"`
}

func (m *messageContent) UnmarshalJSON(data []byte) error {
	*m = messageContent{}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	if data[0] == '"' {
		return json.Unmarshal(data, &m.Text)
	}
	var parts []contentPart
	if err := json.Unmarshal(data, &parts); err != nil {
		return err
	}
	var text, thinking strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "text":
			text.WriteString(part.Text)
		case "thinking":
			thinking.WriteString(partsText(part.Thinking))
		}
	}
	m.Text, m.Thinking = text.String(), thinking.String()
	return nil
}

// partsText reads the text of a nested "thinking" value, which is a list of
// text parts in Mistral's API. A plain string is accepted as well.
func partsText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(part.Text)
	}
	return b.String()
}

type rawChoice struct {
	Index        int        `json:"index"`
	Message      rawMessage `json:"message"`
	Delta        rawMessage `json:"delta"`
	FinishReason string     `json:"finish_reason,omitempty"`
}

type rawEnvelope struct {
	Choices []rawChoice      `json:"choices"`
	Usage   *api.OpenAIUsage `json:"usage,omitempty"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

func (c *Client) reasoningText(m rawMessage) string {
	for _, field := range c.cfg.Settings.ReasoningFields {
		switch field {
		case "reasoning_content":
			if m.ReasoningContent != "" {
				return m.ReasoningContent
			}
		case "reasoning":
			if m.Reasoning != "" {
				return m.Reasoning
			}
		case "reasoning_text":
			if m.ReasoningText != "" {
				return m.ReasoningText
			}
		}
	}
	return m.Content.Thinking
}

// streamToolIndex gives index-less streamed tool calls a stable index.
//
// OpenAI numbers the calls of a response with "index". Some compatible servers
// (Gemini's OpenAI endpoint among them) leave it out and send each parallel
// call whole in its own chunk, which would put every call at the position it
// has inside its chunk, index 0, and merge them into one. Without an index a
// fragment that carries an id different from the previous call's starts a new
// call; one without an id, or repeating the id, continues the previous call.
type streamToolIndex struct {
	started bool
	lastID  string
	last    int
	next    int
}

func (s *streamToolIndex) resolve(explicit *int, id string) int {
	if explicit != nil {
		s.started, s.last = true, *explicit
		if id != "" {
			s.lastID = id
		}
		s.next = max(s.next, *explicit+1)
		return *explicit
	}
	if !s.started || (id != "" && id != s.lastID) {
		s.started = true
		s.last = s.next
		s.next++
	}
	if id != "" {
		s.lastID = id
	}
	return s.last
}

// decodeToolCalls decodes the tool calls of a message or of a streamed delta.
// For a stream, pass the stream's streamToolIndex to number index-less calls
// across chunks; without it a missing index falls back to the position in raws.
func decodeToolCalls(
	raws []json.RawMessage, indexes *streamToolIndex,
) ([]types.LLMToolCall, []api.ToolCallDelta, error) {
	calls := make([]types.LLMToolCall, 0, len(raws))
	deltas := make([]api.ToolCallDelta, 0, len(raws))
	for i, raw := range raws {
		var tc rawToolCall
		if err := json.Unmarshal(raw, &tc); err != nil {
			// Dropping the entry turns a round that wanted to call a tool into
			// a plain answer, and the agent has no way to notice the action it
			// was told to take went missing. Surface it instead.
			return nil, nil, fmt.Errorf("decode tool call %d: %w", i, err)
		}
		idx := i
		if indexes != nil {
			idx = indexes.resolve(tc.Index, tc.ID)
		} else if tc.Index != nil {
			idx = *tc.Index
		}
		calls = append(calls, types.LLMToolCall{
			ID:       tc.ID,
			Type:     orDefault(tc.Type, "function"),
			Function: types.FunctionCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
		})
		deltas = append(deltas, api.ToolCallDelta{
			Index: idx, ID: tc.ID, Type: tc.Type, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	return calls, deltas, nil
}

func (c *Client) parseResponse(raw []byte) (*types.ChatResponse, error) {
	var env rawEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if env.Error != nil && env.Error.Message != "" {
		return nil, fmt.Errorf("API error: %s", env.Error.Message)
	}
	if len(env.Choices) == 0 {
		return nil, fmt.Errorf("no response from API")
	}
	choice := env.Choices[0]
	content, inlineReasoning := splitThinkTags(choice.Message.Content.Text)
	result := &types.ChatResponse{
		Content:          content,
		ReasoningContent: c.reasoningText(choice.Message),
		FinishReason:     choice.FinishReason,
	}
	if result.ReasoningContent == "" {
		result.ReasoningContent = inlineReasoning
	}
	if len(choice.Message.ToolCalls) > 0 {
		calls, _, err := decodeToolCalls(choice.Message.ToolCalls, nil)
		if err != nil {
			return nil, err
		}
		result.ToolCalls = calls
	}
	usage := api.OpenAIUsage{}
	if env.Usage != nil {
		usage = *env.Usage
	}
	result.Usage = usage.ToTokenUsage()
	return result, nil
}

// splitThinkTags handles models that inline their chain of thought as a
// leading <think>...</think> block instead of a reasoning field.
func splitThinkTags(content string) (answer, reasoning string) {
	const start, end = "<think>", "</think>"
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, start) {
		return content, ""
	}
	idx := strings.LastIndex(trimmed, end)
	if idx < 0 {
		// Unterminated: the whole output is (truncated) reasoning.
		return "", strings.TrimSpace(trimmed[len(start):])
	}
	return strings.TrimSpace(trimmed[idx+len(end):]), strings.TrimSpace(trimmed[len(start):idx])
}

func (c *Client) processStream(ctx context.Context, body io.Reader, ch chan<- types.StreamResponse) {
	assembler := api.NewStreamAssembler(ctx, c.cfg.Endpoint.Model)
	reader := api.NewSSEReader(body)
	toolIndexes := &streamToolIndex{}

	for {
		// The consumer went away (client disconnect, aborted agent turn): stop
		// decoding a stream nobody will read instead of spinning to EOF.
		if assembler.Aborted() {
			return
		}
		event, err := reader.ReadEvent()
		if err != nil {
			if err == io.EOF {
				assembler.EndAtEOF(ch)
			} else {
				assembler.Fail(ch, err)
			}
			return
		}
		if event == nil {
			continue
		}
		if event.Done {
			assembler.End(ch)
			return
		}
		if len(event.Data) == 0 {
			continue
		}

		var env rawEnvelope
		if err := json.Unmarshal(event.Data, &env); err != nil {
			// Fail rather than skip. A chunk we cannot decode is a hole in the
			// answer: skipping it ends the stream on EOF, which reaches the
			// caller as a *successful* truncated reply.
			assembler.Fail(ch, fmt.Errorf("decode stream chunk: %w", err))
			return
		}
		if env.Error != nil && env.Error.Message != "" {
			assembler.Fail(ch, fmt.Errorf("API stream error: %s", env.Error.Message))
			return
		}
		if env.Usage != nil {
			assembler.SetUsage(env.Usage.ToTokenUsage())
		}
		if len(env.Choices) == 0 {
			continue
		}
		choice := env.Choices[0]
		delta := api.Delta{
			Reasoning:    c.reasoningText(choice.Delta),
			Content:      choice.Delta.Content.Text,
			FinishReason: choice.FinishReason,
		}
		if len(choice.Delta.ToolCalls) > 0 {
			_, deltas, err := decodeToolCalls(choice.Delta.ToolCalls, toolIndexes)
			if err != nil {
				assembler.Fail(ch, err)
				return
			}
			delta.ToolCalls = deltas
		}
		assembler.Process(ch, delta)
	}
}
