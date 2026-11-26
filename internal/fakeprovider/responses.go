package fakeprovider

import (
	"encoding/json"
	"net/http"
)

type responsesRequest struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions"`
	Input        json.RawMessage `json:"input"`
	Tools        []struct {
		Name string `json:"name"`
	} `json:"tools"`
	Stream bool `json:"stream"`
}

// inputItem is one element of a Responses `input` array: a message, or a
// function call and its output carried back from an earlier turn.
type inputItem struct {
	Type      string  `json:"type"`
	Role      string  `json:"role"`
	Content   content `json:"content"`
	Name      string  `json:"name"`
	Arguments string  `json:"arguments"`
	Output    string  `json:"output"`
}

func (r responsesRequest) prompt() (Prompt, error) {
	p := Prompt{Endpoint: Responses, Model: r.Model}
	if r.Instructions != "" {
		p.Messages = append(p.Messages, Message{Role: "system", Text: r.Instructions})
	}
	for _, t := range r.Tools {
		p.Tools = append(p.Tools, t.Name)
	}
	if len(r.Input) == 0 {
		return p, nil
	}
	var s string
	if err := json.Unmarshal(r.Input, &s); err == nil {
		p.Messages = append(p.Messages, Message{Role: "user", Text: s})
		return p, nil
	}
	var items []inputItem
	if err := json.Unmarshal(r.Input, &items); err != nil {
		return p, err
	}
	for _, it := range items {
		switch it.Type {
		case "function_call":
			p.Messages = append(p.Messages, Message{Role: "assistant", Text: it.Name + " " + it.Arguments})
		case "function_call_output":
			p.Messages = append(p.Messages, Message{Role: "tool", Text: it.Output})
		default:
			p.Messages = append(p.Messages, Message{Role: it.Role, Text: string(it.Content)})
		}
	}
	return p, nil
}

type outputText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type outputItem struct {
	Type      string       `json:"type"`
	ID        string       `json:"id"`
	Status    string       `json:"status"`
	Role      string       `json:"role,omitempty"`
	Content   []outputText `json:"content,omitempty"`
	CallID    string       `json:"call_id,omitempty"`
	Name      string       `json:"name,omitempty"`
	Arguments *string      `json:"arguments,omitempty"`
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type responseObject struct {
	ID        string          `json:"id"`
	Object    string          `json:"object"`
	CreatedAt int             `json:"created_at"`
	Status    string          `json:"status"`
	Model     string          `json:"model"`
	Output    []outputItem    `json:"output"`
	Usage     *responsesUsage `json:"usage"`
}

func (p *Provider) responses(w http.ResponseWriter, body []byte) (int, any) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return badRequest(err)
	}
	prompt, err := req.prompt()
	if err != nil {
		return badRequest(err)
	}
	reply, ok := p.generate(prompt)
	if !ok {
		return unscripted(prompt)
	}
	reply = p.resolve(prompt, reply)

	resp := responseObject{
		ID: p.nextID("resp"), Object: "response", CreatedAt: created, Status: "completed", Model: req.Model,
		Usage: &responsesUsage{InputTokens: reply.Usage.Input, OutputTokens: reply.Usage.Output, TotalTokens: reply.Usage.Input + reply.Usage.Output},
	}
	if reply.Text != "" {
		resp.Output = append(resp.Output, outputItem{
			Type: "message", ID: p.nextID("msg"), Status: "completed", Role: "assistant",
			Content: []outputText{{Type: "output_text", Text: reply.Text, Annotations: []any{}}},
		})
	}
	for _, c := range reply.ToolCalls {
		resp.Output = append(resp.Output, outputItem{
			Type: "function_call", ID: p.nextID("fc"), Status: "completed", CallID: c.ID, Name: c.Name, Arguments: &c.Arguments,
		})
	}
	if resp.Output == nil {
		resp.Output = []outputItem{}
	}

	if req.Stream {
		streamResponse(w, resp)
		return 0, nil
	}
	return http.StatusOK, resp
}

// streamResponse writes resp as Responses stream events: response.created,
// then for each output item its added event, its text or argument deltas
// and its done events, then response.completed with the whole response.
func streamResponse(w http.ResponseWriter, resp responseObject) {
	s := newSSE(w)
	seq := 0
	emit := func(fields map[string]any) {
		fields["sequence_number"] = seq
		seq++
		s.event(fields["type"].(string), fields)
	}

	pending := resp
	pending.Status, pending.Output, pending.Usage = "in_progress", []outputItem{}, nil
	emit(map[string]any{"type": "response.created", "response": pending})

	for i, item := range resp.Output {
		added := item
		added.Status = "in_progress"
		switch item.Type {
		case "message":
			added.Content = []outputText{}
			emit(map[string]any{"type": "response.output_item.added", "output_index": i, "item": added})
			part := outputText{Type: "output_text", Text: "", Annotations: []any{}}
			emit(map[string]any{"type": "response.content_part.added", "item_id": item.ID, "output_index": i, "content_index": 0, "part": part})
			text := item.Content[0].Text
			for _, piece := range pieces(text) {
				emit(map[string]any{"type": "response.output_text.delta", "item_id": item.ID, "output_index": i, "content_index": 0, "delta": piece})
			}
			emit(map[string]any{"type": "response.output_text.done", "item_id": item.ID, "output_index": i, "content_index": 0, "text": text})
			emit(map[string]any{"type": "response.content_part.done", "item_id": item.ID, "output_index": i, "content_index": 0, "part": item.Content[0]})
		case "function_call":
			empty := ""
			added.Arguments = &empty
			emit(map[string]any{"type": "response.output_item.added", "output_index": i, "item": added})
			for _, piece := range pieces(*item.Arguments) {
				emit(map[string]any{"type": "response.function_call_arguments.delta", "item_id": item.ID, "output_index": i, "delta": piece})
			}
			emit(map[string]any{"type": "response.function_call_arguments.done", "item_id": item.ID, "output_index": i, "arguments": *item.Arguments})
		}
		emit(map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
	}
	emit(map[string]any{"type": "response.completed", "response": resp})
}
