package fakeprovider

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

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string  `json:"role"`
		Content content `json:"content"`
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

func (r chatRequest) prompt() Prompt {
	p := Prompt{Endpoint: ChatCompletions, Model: r.Model}
	for _, m := range r.Messages {
		p.Messages = append(p.Messages, Message{Role: m.Role, Text: string(m.Content)})
	}
	for _, t := range r.Tools {
		p.Tools = append(p.Tools, t.Function.Name)
	}
	return p
}

type chatFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type chatToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function chatFunction `json:"function"`
}

type chatMessage struct {
	Role      string         `json:"role,omitempty"`
	Content   *string        `json:"content,omitempty"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type chatChoice struct {
	Index        int          `json:"index"`
	Message      *chatMessage `json:"message,omitempty"`
	Delta        *chatMessage `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int          `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

func (r Reply) chatUsage() *chatUsage {
	return &chatUsage{PromptTokens: r.Usage.Input, CompletionTokens: r.Usage.Output, TotalTokens: r.Usage.Input + r.Usage.Output}
}

func (r Reply) finishReason() *string {
	reason := "stop"
	if len(r.ToolCalls) > 0 {
		reason = "tool_calls"
	}
	return &reason
}

func (p *Provider) chatCompletions(w http.ResponseWriter, body []byte) (int, any) {
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
func (p *Provider) streamChat(w http.ResponseWriter, req chatRequest, id string, reply Reply) {
	s := newSSE(w)
	chunk := func(c chatChoice) chatCompletion {
		return chatCompletion{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model, Choices: []chatChoice{c}}
	}
	empty := ""
	s.data(chunk(chatChoice{Delta: &chatMessage{Role: "assistant", Content: &empty}}))
	for _, piece := range pieces(reply.Text) {
		s.data(chunk(chatChoice{Delta: &chatMessage{Content: &piece}}))
	}
	for i, c := range reply.ToolCalls {
		s.data(chunk(chatChoice{Delta: &chatMessage{ToolCalls: []chatToolCall{{
			Index: &i, ID: c.ID, Type: "function", Function: chatFunction{Name: c.Name},
		}}}}))
		for _, piece := range pieces(c.Arguments) {
			s.data(chunk(chatChoice{Delta: &chatMessage{ToolCalls: []chatToolCall{{
				Index: &i, Function: chatFunction{Arguments: piece},
			}}}}))
		}
	}
	s.data(chunk(chatChoice{Delta: &chatMessage{}, FinishReason: reply.finishReason()}))
	if req.StreamOptions.IncludeUsage {
		s.data(chatCompletion{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model, Choices: []chatChoice{}, Usage: reply.chatUsage()})
	}
	s.raw("[DONE]")
}

func badRequest(err error) (int, any) {
	var b errorBody
	b.Error.Message = "fakeprovider: " + err.Error()
	b.Error.Type = "invalid_request_error"
	return http.StatusBadRequest, b
}

// unscripted answers a request no rule matched; the test has already been
// failed by generate.
func unscripted(prompt Prompt) (int, any) {
	var b errorBody
	b.Error.Message = "fakeprovider: no Reply rule matches this request:\n" + prompt.String()
	b.Error.Type = "unscripted"
	return http.StatusInternalServerError, b
}
