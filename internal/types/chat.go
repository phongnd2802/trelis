package types

type ChatResponse struct {
	Content          string
	ReasoningContent string
}

type ResponseType string

type StreamingResponse struct {
	ID           string
	ResponseType ResponseType
	Content      string
	Done         bool
	Data         map[string]interface{}
	FinishReason string
}
