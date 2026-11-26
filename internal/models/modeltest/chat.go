package modeltest

import (
	"encoding/json"
	"net/http"
	"strings"
)

// created is the fixed `created` timestamp of every answer, so answers do
// not depend on the clock.
const created = 1700000000

// content is a Chat Completions or Responses message content: a string or
// an array of parts with text.
type content string

func (c *content) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*c = content(s)
		return nil
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return err
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	*c = content(strings.Join(texts, "\n"))
	return nil
}

// chatRequest is the part of a Chat Completions request the provider reads.
type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role      string         `json:"role"`
		Content   content        `json:"content"`
		ToolCalls []chatToolCall `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	Stream        bool `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// prompt reduces r to what a Match sees.
func (r chatRequest) prompt() Prompt {
	p := Prompt{Endpoint: ChatCompletions, Model: r.Model}
	for _, m := range r.Messages {
		if m.Content != "" || len(m.ToolCalls) == 0 {
			p.Messages = append(p.Messages, Message{Role: m.Role, Text: string(m.Content)})
		}
		// One Message per call, as a Responses function_call input item
		// becomes, so a rule matches the same history on both endpoints.
		for _, c := range m.ToolCalls {
			p.Messages = append(p.Messages, Message{Role: m.Role, Text: c.Function.Name + " " + c.Function.Arguments})
		}
	}
	for _, t := range r.Tools {
		p.Tools = append(p.Tools, t.Function.Name)
	}
	return p
}

// chatFunction is a tool call's function, in a request or an answer.
type chatFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// chatToolCall is one tool call; Index is set only in stream deltas.
type chatToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function chatFunction `json:"function"`
}

// chatMessage is the assistant message of a non-streamed answer; content
// is null when the model only calls tools.
type chatMessage struct {
	Role      string         `json:"role"`
	Content   *string        `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

// chatDelta is the part of the assistant message one stream chunk adds.
type chatDelta struct {
	Role      string         `json:"role,omitempty"`
	Content   *string        `json:"content,omitempty"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

// chatChoice carries a Message in an answer or a Delta in a stream chunk.
type chatChoice struct {
	Index        int          `json:"index"`
	Message      *chatMessage `json:"message,omitempty"`
	Delta        *chatDelta   `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

// chatUsage is Chat Completions token accounting.
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatCompletion is an answer or, with Object chat.completion.chunk, one
// stream chunk.
type chatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int          `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

func (r Reply) chatUsage() *chatUsage {
	return &chatUsage{PromptTokens: r.Usage.Input, CompletionTokens: r.Usage.Output, TotalTokens: r.Usage.total()}
}

// finishReason is tool_calls when r calls a tool, else stop.
func (r Reply) finishReason() *string {
	reason := "stop"
	if len(r.ToolCalls) > 0 {
		reason = "tool_calls"
	}
	return &reason
}

// chatCompletions answers POST /v1/chat/completions in the shape of
// https://platform.openai.com/docs/api-reference/chat/object (and, streamed,
// .../chat/streaming).
func (p *FakeProvider) chatCompletions(w http.ResponseWriter, body []byte) (int, any) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return badRequest(err)
	}
	prompt := req.prompt()
	reply, ok := p.generate(prompt)
	if !ok {
		return unscripted(prompt)
	}
	reply = p.resolve(prompt, reply)
	id := p.nextID("chatcmpl")

	if req.Stream {
		p.streamChat(w, req, id, reply)
		return 0, nil
	}
	msg := &chatMessage{Role: "assistant"}
	if reply.Text != "" || len(reply.ToolCalls) == 0 {
		msg.Content = &reply.Text
	}
	for _, c := range reply.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, chatToolCall{ID: c.ID, Type: "function", Function: chatFunction{Name: c.Name, Arguments: c.Arguments}})
	}
	return http.StatusOK, chatCompletion{
		ID: id, Object: "chat.completion", Created: created, Model: req.Model,
		Choices: []chatChoice{{Message: msg, FinishReason: reply.finishReason()}},
		Usage:   reply.chatUsage(),
	}
}

// streamChat writes reply as chat.completion.chunk events: the role, the
// text a few words at a time, each tool call's name then its arguments in
// pieces, the finish reason, and usage when the request asked for it.
func (p *FakeProvider) streamChat(w http.ResponseWriter, req chatRequest, id string, reply Reply) {
	s := newSSE(w)
	chunk := func(c chatChoice) chatCompletion {
		return chatCompletion{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model, Choices: []chatChoice{c}}
	}
	empty := ""
	s.data(chunk(chatChoice{Delta: &chatDelta{Role: "assistant", Content: &empty}}))
	for _, piece := range pieces(reply.Text) {
		s.data(chunk(chatChoice{Delta: &chatDelta{Content: &piece}}))
	}
	for i, c := range reply.ToolCalls {
		s.data(chunk(chatChoice{Delta: &chatDelta{ToolCalls: []chatToolCall{{
			Index: &i, ID: c.ID, Type: "function", Function: chatFunction{Name: c.Name},
		}}}}))
		for _, piece := range pieces(c.Arguments) {
			s.data(chunk(chatChoice{Delta: &chatDelta{ToolCalls: []chatToolCall{{
				Index: &i, Function: chatFunction{Arguments: piece},
			}}}}))
		}
	}
	s.data(chunk(chatChoice{Delta: &chatDelta{}, FinishReason: reply.finishReason()}))
	if req.StreamOptions.IncludeUsage {
		last := chunk(chatChoice{})
		last.Choices, last.Usage = []chatChoice{}, reply.chatUsage()
		s.data(last)
	}
	s.raw("[DONE]")
}

// badRequest answers a body the provider cannot read.
func badRequest(err error) (int, any) {
	return http.StatusBadRequest, newError("modeltest: "+err.Error(), "invalid_request_error")
}

// unscripted answers a request no rule matched; the test has already been
// failed by generate.
func unscripted(prompt Prompt) (int, any) {
	return http.StatusInternalServerError, newError("modeltest: unexpected extra call: the script is used up and no Reply rule matches this request:\n"+prompt.String(), "unscripted")
}
