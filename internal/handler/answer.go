package handler

import (
	"net/http"

	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// AnswerHandler processes HTTP requests that answer a question from the
// Documents the caller can read.
type AnswerHandler struct {
	service interfaces.AnswerService
}

// NewAnswerHandler creates a new Pipeline Answer handler.
func NewAnswerHandler(service interfaces.AnswerService) *AnswerHandler {
	return &AnswerHandler{service: service}
}

// Answer answers POST /answer with a Pipeline Answer: it retrieves the
// Documents the caller can read and asks models.chat for one answer that
// cites the retrieved Chunks. debug also returns the retrieved Chunks, the
// trace and the chat call's token usage. A caller who can read nothing gets
// the fixed no-source answer, never a 404, so the answer reveals nothing
// about what exists.
//
//	@Summary		Answer a question from readable Documents
//	@Description	Retrieves the Documents the caller can read, then asks models.chat for one answer that cites the retrieved Chunks. Sources are numbered [1]..[n] in retrieval order; each claim is cited with its source numbers in square brackets, the answer text keeps the valid markers, and citations lists each cited source once in order of first appearance. A marker outside 1..n is removed from the text and never cited; a marker inside Markdown code or a Markdown link is left untouched and never cited. When retrieval finds nothing the answer is a fixed sentence in the question's language and no chat call is made. debug adds the retrieved Chunks (exactly as POST /retrieve answers them), the trace with a final generate step, and the chat call's token usage. Malformed JSON, unknown fields, invalid values and the retrieval rules are 400; a body over 64 KiB is 413; without models.chat the route answers 503 before any retrieval, and retrieval's own 503 (models.embedding for dense, models.rerank for rerank) passes through; a chat failure is a generic 500.
//	@Tags			answering
//	@Accept			json
//	@Produce		json
//	@Security		bearerAuth
//	@Param			request	body		types.AnswerRequest	true	"The query, how many Chunks to retrieve, which Retrieval Techniques to run and whether to return the debug object"
//	@Success		200		{object}	handler.DataResponse{data=types.AnswerResult}
//	@Failure		400		{object}	handler.ErrorResponse	"Body that is not one JSON object, an unknown field or wrong value type, a blank or too-long query, an out-of-range top_k or an invalid retrieval_config"
//	@Failure		401		{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		413		{object}	handler.ErrorResponse	"Request body larger than 64 KiB"
//	@Failure		500		{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Failure		503		{object}	handler.ErrorResponse	"models.chat is not configured, or retrieval is unavailable (models.embedding for dense, models.rerank for rerank)"
//	@Router			/answer [post]
func (h *AnswerHandler) Answer(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
	req, appErr := decodeJSONRequest[types.AnswerRequest](r)
	if appErr != nil {
		writeError(w, r, appErr)
		return
	}
	result, err := h.service.Answer(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(r.Context(), w, http.StatusOK, result)
}
