package types

// Chat message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ChatMessage is one message of a conversation sent to a chat model, in a
// protocol-neutral shape.
type ChatMessage struct {
	Role    string
	Content string
	// ToolCalls are the calls an assistant message asked for.
	ToolCalls []ToolCall
	// ToolCallID links a tool message to the call it answers.
	ToolCallID string
}

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string
	// Parameters is the JSON Schema of the arguments.
	Parameters map[string]any
}

// ToolCall is one call the model asked for.
type ToolCall struct {
	ID   string
	Name string
	// Arguments is the JSON object the model wrote, as sent.
	Arguments string
}

// Usage is the token count the provider reported for one call. It is zero
// when the provider reported none.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ChatOptions tune one chat call. The zero value sends only the model and
// the messages.
type ChatOptions struct {
	Temperature *float64
	MaxTokens   int
	Tools       []Tool
	// ToolChoice is "auto", "none", "required" or the name of a tool to
	// force; empty leaves it to the provider.
	ToolChoice string
}

// ChatResponse is the result of a non-streaming chat call.
type ChatResponse struct {
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	FinishReason     string
	Usage            Usage
}

// StreamingResponse is one event of a streaming chat call. Content and
// ReasoningContent carry deltas. The last event has Done set and carries the
// assembled ToolCalls, the FinishReason and the Usage, or Err if the stream
// failed; the channel is closed after it.
type StreamingResponse struct {
	Content          string
	ReasoningContent string
	Done             bool
	ToolCalls        []ToolCall
	FinishReason     string
	Usage            Usage
	Err              error
}
