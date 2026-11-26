package openaichat

import "github.com/phongnd2802/trelis/internal/types"

// Wire shapes of the Chat Completions protocol.

type request struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Temperature   *float64       `json:"temperature,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Tools         []wireTool     `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role string `json:"role"`
	// Content is null on an assistant message that only calls tools.
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type wireToolCall struct {
	// Index orders tool-call deltas in a stream.
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireCallFunc `json:"function"`
}

type wireCallFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type completion struct {
	Choices []struct {
		Message      responseMessage `json:"message"`
		FinishReason string          `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

type responseMessage struct {
	Content string `json:"content"`
	// ReasoningContent is sent by DeepSeek, vLLM and others.
	ReasoningContent string         `json:"reasoning_content"`
	ToolCalls        []wireToolCall `json:"tool_calls"`
}

type chunk struct {
	Choices []struct {
		Delta        responseMessage `json:"delta"`
		FinishReason string          `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	// Error is how some providers report a failure after the stream began.
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (u *wireUsage) toUsage() types.Usage {
	if u == nil {
		return types.Usage{}
	}
	return types.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
}

func toWireMessages(msgs []types.ChatMessage) []wireMessage {
	out := make([]wireMessage, len(msgs))
	for i, m := range msgs {
		w := wireMessage{Role: m.Role, ToolCallID: m.ToolCallID}
		if m.Content != "" || len(m.ToolCalls) == 0 {
			content := m.Content
			w.Content = &content
		}
		for _, tc := range m.ToolCalls {
			w.ToolCalls = append(w.ToolCalls, wireToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: wireCallFunc{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		out[i] = w
	}
	return out
}

func toWireTools(tools []types.Tool) []wireTool {
	var out []wireTool
	for _, t := range tools {
		out = append(out, wireTool{
			Type:     "function",
			Function: wireFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	return out
}

// toWireToolChoice passes the keywords through and turns a tool name into
// the object form that forces that tool.
func toWireToolChoice(choice string) any {
	switch choice {
	case "":
		return nil
	case "auto", "none", "required":
		return choice
	default:
		return map[string]any{"type": "function", "function": map[string]string{"name": choice}}
	}
}

func fromWireToolCalls(calls []wireToolCall) []types.ToolCall {
	var out []types.ToolCall
	for _, c := range calls {
		out = append(out, types.ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	return out
}
