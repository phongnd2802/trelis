package types

import "github.com/google/uuid"

// AnswerRequest is the body of POST /answer: the fields of a
// RetrievalRequest, so it accepts exactly what POST /retrieve accepts and
// validates and fills defaults through its promoted Validate, plus the debug
// flag that asks for the retrieved Chunks, the trace and the token usage.
type AnswerRequest struct {
	RetrievalRequest
	// Debug asks the answer to carry the retrieved Chunks, the trace and the
	// chat call's token usage; false leaves the debug object out entirely.
	Debug bool `json:"debug"`
}

// TraceStepGenerate is the answer's step that called the chat role, timed
// like the retrieval steps so a caller can see where the latency went.
const TraceStepGenerate = "generate"

// Citation is one source a Pipeline Answer cites: the marker in the answer
// text and the Chunk and Document it opens.
type Citation struct {
	// Marker is the source number the answer text cites, e.g. 2 for [2].
	Marker int `json:"marker"`
	// ChunkID is the retrieved Chunk the marker stands for.
	ChunkID uuid.UUID `json:"chunk_id"`
	// DocumentID is the Document that Chunk belongs to.
	DocumentID uuid.UUID `json:"document_id"`
}

// AnswerDebug is what a POST /answer with debug: true adds to the answer: the
// retrieved Chunks exactly as POST /retrieve would answer them, the trace of
// the retrieval steps plus the generate step, and what the chat call cost.
type AnswerDebug struct {
	Results []RetrievedChunk `json:"results"`
	Trace   []TraceStep      `json:"trace"`
	// Usage is the chat call's token accounting; zeros when no call was made.
	Usage TokenUsage `json:"usage"`
}

// AnswerResult is the payload of a successful POST /answer: the answer text
// with its valid source markers kept, the cited sources in order of first
// appearance, the chat model's finish reason, and the debug object when the
// request asked for it.
type AnswerResult struct {
	Answer string `json:"answer"`
	// Citations is never null: an answer with no source cites nothing.
	Citations    []Citation `json:"citations"`
	FinishReason string     `json:"finish_reason"`
	// Debug is present only when the request set debug; nil leaves the key
	// out.
	Debug *AnswerDebug `json:"debug,omitempty"`
}

// AnswerEventType names one kind of Answer Event.
type AnswerEventType string

// The Answer Event types of a streamed Pipeline Answer, in the order the
// service emits them. started comes first, token events follow while the chat
// role streams, one citation event precedes done for every cited source, and
// done or error is terminal.
const (
	AnswerEventStarted  AnswerEventType = "started"
	AnswerEventToken    AnswerEventType = "token"
	AnswerEventCitation AnswerEventType = "citation"
	AnswerEventDone     AnswerEventType = "done"
	AnswerEventError    AnswerEventType = "error"
)

// AnswerEvent is one Answer Event: one event of a streamed Pipeline Answer.
// Seq numbers the events of one answer from 1, so a client resumes with the
// seq of the last event it saw.
type AnswerEvent struct {
	Seq  int64
	Type AnswerEventType
	// Data is the event's JSON payload; the handler frames it as the data
	// line and never changes it.
	Data any
}

// AnswerSubscription is one read of an answer's buffered events: the Answer
// Events after the requested seq, a channel closed when more arrive or the
// stream ends, and whether it has ended.
type AnswerSubscription struct {
	Events []AnswerEvent
	Wait   <-chan struct{}
	Done   bool
}

// AnswerStartedEvent is the first event of a streamed answer: the answer ID a
// client uses to resume.
type AnswerStartedEvent struct {
	AnswerID uuid.UUID `json:"answer_id"`
}

// AnswerTokenEvent is one raw text delta from the chat role. Concatenating
// the token events gives the model's full reply, before citation markers are
// cleaned.
type AnswerTokenEvent struct {
	Text string `json:"text"`
}

// AnswerCitationEvent is one source the answer cites: the marker in the text
// and the Chunk and Document it opens.
type AnswerCitationEvent struct {
	Marker     int       `json:"marker"`
	ChunkID    uuid.UUID `json:"chunk_id"`
	DocumentID uuid.UUID `json:"document_id"`
}

// AnswerDoneEvent is the terminal event of a successful streamed answer. The
// answer is the cleaned text, exactly as the JSON answer would return it.
type AnswerDoneEvent struct {
	Answer       string `json:"answer"`
	FinishReason string `json:"finish_reason"`
	// Debug is present only when the request set debug.
	Debug *AnswerDebug `json:"debug,omitempty"`
}

// AnswerErrorEvent is the terminal event of a failed streamed answer. Message
// is generic: the cause is logged, never sent to the client.
type AnswerErrorEvent struct {
	Message string `json:"message"`
}
