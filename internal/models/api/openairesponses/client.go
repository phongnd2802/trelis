// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/openairesponses/client.go.
// Trelis drops reasoning metadata and summaries, prompt caching, multimodal
// retry, request/usage logging, raw packet dumps and GetModelID (roles come
// from config, not a model database).
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

package openairesponses

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/types"
)

const defaultPath = "/responses"

// Client talks the Responses protocol to one endpoint.
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

// Chat performs a non-streaming response creation.
func (c *Client) Chat(ctx context.Context, messages []api.Message, opts *api.Options) (*types.ChatResponse, error) {
	raw, err := c.cfg.Endpoint.PostJSON(ctx, defaultPath, c.BuildRequestBody(messages, opts, false), api.DefaultChatTimeout)
	if err != nil {
		return nil, fmt.Errorf("create response: %w", err)
	}
	return c.parseResponse(raw)
}

// ChatStream performs a streaming response creation.
func (c *Client) ChatStream(
	ctx context.Context, messages []api.Message, opts *api.Options,
) (<-chan types.StreamResponse, error) {
	ctx, cancel := api.WithLLMTimeout(ctx, api.DefaultStreamTimeout)

	resp, err := c.send(ctx, messages, opts, true)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create response stream: %w", err)
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

type rawContentPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

// rawOutputItem is the union of the output item shapes this package reads:
// message and function_call.
type rawOutputItem struct {
	Type      string           `json:"type"`
	Content   []rawContentPart `json:"content,omitempty"`
	CallID    string           `json:"call_id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Arguments string           `json:"arguments,omitempty"`
}

// rawUsage is the Responses token accounting: input and output where Chat
// Completions says prompt and completion.
type rawUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func (u rawUsage) toTokenUsage() types.TokenUsage {
	out := types.TokenUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}

type rawError struct {
	Message string `json:"message"`
}

type rawResponse struct {
	Status            string `json:"status,omitempty"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details,omitempty"`
	Output []json.RawMessage `json:"output,omitempty"`
	Usage  *rawUsage         `json:"usage,omitempty"`
	Error  *rawError         `json:"error,omitempty"`
}

// finishReason maps the response status onto the Chat Completions vocabulary
// the rest of the engine understands.
func (r *rawResponse) finishReason(hasToolCalls bool) string {
	// An incomplete response is checked first: a tool call cut off by
	// max_output_tokens carries truncated arguments and must not be run.
	if r != nil && r.Status == "incomplete" && r.IncompleteDetails != nil {
		switch r.IncompleteDetails.Reason {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		}
	}
	if hasToolCalls {
		return "tool_calls"
	}
	return "stop"
}

// itemText joins the answer text of an output message. A refusal is answer
// text here: it is what the caller has to show when the model declines.
func itemText(item rawOutputItem) string {
	var b strings.Builder
	for _, part := range item.Content {
		switch part.Type {
		case "output_text":
			b.WriteString(part.Text)
		case "refusal":
			b.WriteString(part.Refusal)
		}
	}
	return b.String()
}

func (c *Client) parseResponse(raw []byte) (*types.ChatResponse, error) {
	var env rawResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if env.Error != nil && env.Error.Message != "" {
		return nil, fmt.Errorf("API error: %s", env.Error.Message)
	}
	if env.Status == "failed" {
		return nil, fmt.Errorf("API error: response failed")
	}

	result := &types.ChatResponse{}
	var content []string
	for _, rawItem := range env.Output {
		// An output item we cannot read may be the round's tool call. Dropping
		// it would hand the caller what looks like a plain answer, so fail
		// instead of moving on.
		var item rawOutputItem
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return nil, fmt.Errorf("decode output item: %w", err)
		}
		switch item.Type {
		case "message":
			content = append(content, itemText(item))
		case "function_call":
			result.ToolCalls = append(result.ToolCalls, types.LLMToolCall{
				ID:       item.CallID,
				Type:     "function",
				Function: types.FunctionCall{Name: item.Name, Arguments: item.Arguments},
			})
		}
	}
	result.Content = strings.Join(content, "")
	result.FinishReason = env.finishReason(len(result.ToolCalls) > 0)
	usage := rawUsage{}
	if env.Usage != nil {
		usage = *env.Usage
	}
	result.Usage = usage.toTokenUsage()
	return result, nil
}

// --- streaming ---

// rawStreamEvent is the union of the fields this package reads from the
// Responses SSE events. Every event carries "type".
type rawStreamEvent struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta,omitempty"`
	OutputIndex int             `json:"output_index,omitempty"`
	Item        json.RawMessage `json:"item,omitempty"`
	Response    *rawResponse    `json:"response,omitempty"`
	// Arguments is set on response.function_call_arguments.done.
	Arguments string `json:"arguments,omitempty"`
	// Message is set on the top-level "error" event.
	Message string `json:"message,omitempty"`
}

func (c *Client) processStream(ctx context.Context, body io.Reader, ch chan<- types.StreamResponse) {
	assembler := api.NewStreamAssembler(ctx, c.cfg.Endpoint.Model)
	reader := api.NewSSEReader(body)
	// argsSeen tracks which calls streamed their arguments, so the whole
	// arguments a done event may carry are not appended a second time.
	argsSeen := make(map[int]bool)

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
			// Responses terminates with response.completed; [DONE] without one
			// leaves the reply incomplete.
			assembler.EndAtEOF(ch)
			return
		}
		if len(event.Data) == 0 {
			continue
		}

		var ev rawStreamEvent
		if err := json.Unmarshal(event.Data, &ev); err != nil {
			// An unknown event *type* is routine and the switch below ignores
			// it, but a payload that is not JSON is a hole in the answer:
			// skipping it runs the loop to EOF, which reaches the caller as a
			// successful short reply. Fail, as the Completions loop does.
			assembler.Fail(ch, fmt.Errorf("decode stream chunk: %w", err))
			return
		}

		switch ev.Type {
		case "response.output_text.delta", "response.refusal.delta":
			if ev.Delta != "" {
				assembler.Process(ch, api.Delta{Content: ev.Delta})
			}
		case "response.output_item.added":
			item, ok, err := streamItem(ev.Item)
			if err != nil {
				assembler.Fail(ch, err)
				return
			}
			if !ok || item.Type != "function_call" {
				continue
			}
			// output_index is the assembler key: argument deltas carry it too,
			// so a call keeps its id and name when its arguments arrive.
			assembler.Process(ch, api.Delta{ToolCalls: []api.ToolCallDelta{{
				Index: ev.OutputIndex, ID: item.CallID, Type: "function", Name: item.Name,
			}}})
		case "response.function_call_arguments.delta":
			if ev.Delta == "" {
				continue
			}
			argsSeen[ev.OutputIndex] = true
			assembler.Process(ch, api.Delta{ToolCalls: []api.ToolCallDelta{{
				Index: ev.OutputIndex, Arguments: ev.Delta,
			}}})
		case "response.function_call_arguments.done":
			// Some gateways send no argument deltas and put the whole string
			// here instead; when deltas did arrive, this would repeat them.
			if ev.Arguments == "" || argsSeen[ev.OutputIndex] {
				continue
			}
			argsSeen[ev.OutputIndex] = true
			assembler.Process(ch, api.Delta{ToolCalls: []api.ToolCallDelta{{
				Index: ev.OutputIndex, Arguments: ev.Arguments,
			}}})
		case "response.output_item.done":
			item, ok, err := streamItem(ev.Item)
			if err != nil {
				assembler.Fail(ch, err)
				return
			}
			if !ok || item.Type != "function_call" || item.Arguments == "" || argsSeen[ev.OutputIndex] {
				continue
			}
			argsSeen[ev.OutputIndex] = true
			assembler.Process(ch, api.Delta{ToolCalls: []api.ToolCallDelta{{
				Index: ev.OutputIndex, ID: item.CallID, Type: "function", Name: item.Name,
				Arguments: item.Arguments,
			}}})
		case "response.completed", "response.incomplete":
			// response.incomplete is the terminal event for a cut-off
			// response: it carries the usage, and finishReason turns its
			// details into the same "length" the non-streaming path reports.
			completeStream(ch, assembler, ev.Response, ev.Type == "response.incomplete")
			return
		case "response.failed":
			msg := "response failed"
			if ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "" {
				msg = ev.Response.Error.Message
			}
			assembler.Fail(ch, fmt.Errorf("API stream error: %s", msg))
			return
		case "error":
			assembler.Fail(ch, fmt.Errorf("API stream error: %s", orDefault(ev.Message, "unknown error")))
			return
		}
	}
}

// streamItem decodes the item carried by an output_item event. An event that
// carries no item is nothing to decode; an item we cannot read may be a whole
// function_call, so the caller must surface it.
func streamItem(raw json.RawMessage) (rawOutputItem, bool, error) {
	if len(raw) == 0 {
		return rawOutputItem{}, false, nil
	}
	var item rawOutputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return rawOutputItem{}, false, fmt.Errorf("decode output item: %w", err)
	}
	return item, true, nil
}

// completeStream handles the terminal response.completed / response.incomplete
// event: the terminal event carries the usage, Process records the mapped
// finish reason, and the assembler's closing chunk carries the assembled tool
// calls. An incomplete response with no usable details is still cut short, so
// it reports "length" rather than a clean stop.
func completeStream(ch chan<- types.StreamResponse, assembler *api.StreamAssembler, resp *rawResponse, incomplete bool) {
	if resp != nil && resp.Usage != nil {
		assembler.SetUsage(resp.Usage.toTokenUsage())
	}
	finishReason := resp.finishReason(len(assembler.OrderedToolCalls()) > 0)
	if incomplete && (finishReason == "stop" || finishReason == "tool_calls") {
		finishReason = "length"
	}
	assembler.Process(ch, api.Delta{FinishReason: finishReason})
	assembler.End(ch)
}
