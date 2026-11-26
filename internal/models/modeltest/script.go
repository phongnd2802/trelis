package modeltest

import (
	"fmt"
	"slices"
	"strings"
)

// Prompt is a generation request reduced to what a [Match] looks at, the
// same for Chat Completions and Responses.
type Prompt struct {
	Endpoint Endpoint
	Model    string
	// Messages are in request order. A Responses `instructions` string is a
	// leading system Message; a tool result (a Chat Completions `tool`
	// message or a Responses `function_call_output`) has Role "tool", and
	// each tool call carried back in the history is an assistant Message
	// with the text "<name> <arguments>".
	Messages []Message
	// Tools are the names of the functions the request offers.
	Tools []string
}

// Message is one turn's role and its text parts joined by newlines.
type Message struct {
	Role string
	Text string
}

// String lists the messages, for failure output.
func (p Prompt) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "model=%q tools=%v\n", p.Model, p.Tools)
	for _, m := range p.Messages {
		fmt.Fprintf(&b, "  %s: %q\n", m.Role, m.Text)
	}
	return b.String()
}

// last returns the final message, or a zero Message.
func (p Prompt) last() Message {
	if len(p.Messages) == 0 {
		return Message{}
	}
	return p.Messages[len(p.Messages)-1]
}

// Match decides whether a [Reply] rule applies to a request.
type Match func(Prompt) bool

// Any matches every request.
func Any() Match { return func(Prompt) bool { return true } }

// LastContains matches when the final message's text contains s.
func LastContains(s string) Match {
	return func(p Prompt) bool { return strings.Contains(p.last().Text, s) }
}

// Contains matches when any message's text contains s.
func Contains(s string) Match {
	return func(p Prompt) bool {
		return slices.ContainsFunc(p.Messages, func(m Message) bool { return strings.Contains(m.Text, s) })
	}
}

// LastRole matches when the final message has the given role, e.g. "tool"
// for the turn after a tool call was answered.
func LastRole(role string) Match {
	return func(p Prompt) bool { return p.last().Role == role }
}

// Model matches requests for the named model.
func Model(name string) Match {
	return func(p Prompt) bool { return p.Model == name }
}

// HasTool matches requests that offer a function with the given name.
func HasTool(name string) Match {
	return func(p Prompt) bool { return slices.Contains(p.Tools, name) }
}

// All matches when every one of ms does.
func All(ms ...Match) Match {
	return func(p Prompt) bool {
		for _, m := range ms {
			if !m(p) {
				return false
			}
		}
		return true
	}
}

// Reply is what the model says: text, tool calls, or both.
type Reply struct {
	Text      string
	ToolCalls []ToolCall
	// Usage overrides the token counts; zero fields are counted from the
	// request and reply (one token per word).
	Usage Usage
}

// ToolCall is one function call the model makes.
type ToolCall struct {
	// ID defaults to a fresh "call_N".
	ID   string
	Name string
	// Arguments is the JSON object passed to the function, as text.
	Arguments string
}

// Usage is token accounting for one reply.
type Usage struct {
	Input  int
	Output int
}

func (u Usage) total() int { return u.Input + u.Output }

// Text is a Reply that only says s.
func Text(s string) Reply { return Reply{Text: s} }

// Call is a Reply that calls the named function with JSON arguments.
func Call(name, arguments string) Reply {
	return Reply{ToolCalls: []ToolCall{{Name: name, Arguments: arguments}}}
}

// resolve fills in defaults for one answer to prompt: tool call ids and
// token counts.
func (p *FakeProvider) resolve(prompt Prompt, r Reply) Reply {
	calls := make([]ToolCall, len(r.ToolCalls))
	for i, c := range r.ToolCalls {
		if c.ID == "" {
			c.ID = p.nextID("call")
		}
		if c.Arguments == "" {
			c.Arguments = "{}"
		}
		calls[i] = c
	}
	r.ToolCalls = calls
	if r.Usage.Input == 0 {
		for _, m := range prompt.Messages {
			r.Usage.Input += countTokens(m.Text)
		}
	}
	if r.Usage.Output == 0 {
		r.Usage.Output = countTokens(r.Text)
		for _, c := range r.ToolCalls {
			r.Usage.Output += countTokens(c.Name + " " + c.Arguments)
		}
	}
	return r
}
