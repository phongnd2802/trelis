package handler

import (
	"net/http"
	"time"

	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// AnswerHandler processes HTTP requests that answer a question from the
// Documents the caller can read.
type AnswerHandler struct {
	service interfaces.AnswerService
	// streamKeepalive is how long an idle answer stream waits before writing
	// a comment frame. Zero uses defaultAnswerStreamKeepalive; tests set it
	// through WithAnswerStreamKeepalive.
	streamKeepalive time.Duration
}

// AnswerHandlerOption configures an AnswerHandler.
type AnswerHandlerOption func(*AnswerHandler)

// NewAnswerHandler creates a new Pipeline Answer handler. opts set the answer
// stream timings.
func NewAnswerHandler(service interfaces.AnswerService, opts ...AnswerHandlerOption) *AnswerHandler {
	h := &AnswerHandler{service: service}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Answer answers POST /answer with a Pipeline Answer: it retrieves the
// Documents the caller can read and asks models.chat for one answer that
// cites the retrieved Chunks. debug also returns the retrieved Chunks, the
// trace and the chat call's token usage. A caller who can read nothing gets
// the fixed no-source answer, never a 404, so the answer reveals nothing
// about what exists. When Accept names text/event-stream the answer is a
// Server-Sent Events stream instead of the JSON envelope: started, one token
// event per model delta, one citation event per cited source, then a terminal
// done (or error) event.
//
//	@Summary		Answer a question from readable Documents
//	@Description	Retrieves the Documents the caller can read, then asks models.chat for one answer that cites the retrieved Chunks. Sources are numbered [1]..[n] in retrieval order; each claim is cited with its source numbers in square brackets, the answer text keeps the valid markers, and citations lists each cited source once in order of first appearance. A marker outside 1..n is removed from the text and never cited; a marker inside Markdown code or a Markdown link is left untouched and never cited. When retrieval finds nothing the answer is a fixed sentence in the question's language and no chat call is made. debug adds the retrieved Chunks (exactly as POST /retrieve answers them), the trace with a final generate step, and the chat call's token usage. With Accept: text/event-stream the answer streams Answer Events as Server-Sent Events instead: id is the per-answer seq and event is started {"answer_id"}, token {"text"} per model delta, citation {"marker","chunk_id","document_id"} after the text, then done {"answer","finish_reason","debug"?}; an error {"message"} replaces done when generation fails, and both are terminal. The chat stream is opened before the SSE response, so a provider rejection is the same JSON 502. A dropped client resumes at GET /answers/{answer_id}/events with Last-Event-ID. Malformed JSON, unknown fields, invalid values and the retrieval rules are 400; a body over 64 KiB is 413; without models.chat the route answers 503 before any retrieval, and retrieval's own 503 (models.embedding for dense, models.rerank for rerank) passes through; a model provider's rejection of a retrieval or opening the chat stream (its credentials, credit, permission or rate limit) is 502 and names the model role in its message, and any other model failure is a generic 500. Every one of these failures answers the JSON error before the SSE headers.
//	@Tags			answering
//	@Accept			json
//	@Produce		json
//	@Produce		text/event-stream
//	@Security		bearerAuth
//	@Param			request	body		types.AnswerRequest	true	"The query, how many Chunks to retrieve, which Retrieval Techniques to run and whether to return the debug object"
//	@Success		200		{object}	handler.DataResponse{data=types.AnswerResult}	"The JSON answer when Accept does not ask for text/event-stream; otherwise Server-Sent Events: started, token..., citation..., then done or error"
//	@Failure		400		{object}	handler.ErrorResponse	"Body that is not one JSON object, an unknown field or wrong value type, a blank or too-long query, an out-of-range top_k or an invalid retrieval_config"
//	@Failure		401		{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		413		{object}	handler.ErrorResponse	"Request body larger than 64 KiB"
//	@Failure		500		{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Failure		502		{object}	handler.ErrorResponse	"A model provider rejected the call because of its credentials, credit, permission or rate limit (an embedding, rerank or chat status 401, 402, 403 or 429); the message names the model role, never the provider's body"
//	@Failure		503		{object}	handler.ErrorResponse	"models.chat is not configured, or retrieval is unavailable (models.embedding for dense, models.rerank for rerank)"
//	@Router			/answer [post]
func (h *AnswerHandler) Answer(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
	req, appErr := decodeJSONRequest[types.AnswerRequest](r)
	if appErr != nil {
		writeError(w, r, appErr)
		return
	}
	if wantsEventStream(r) {
		answerID, err := h.service.StartAnswerStream(r.Context(), req)
		if err != nil {
			writeError(w, r, err)
			return
		}
		h.streamAnswerEvents(w, r, answerID, 0)
		return
	}
	result, err := h.service.Answer(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(r.Context(), w, http.StatusOK, result)
}
