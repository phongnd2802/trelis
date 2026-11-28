package handler

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// ConversationHandler processes HTTP requests about Conversations: the
// questions one Principal asked and the answers they got.
type ConversationHandler struct {
	service interfaces.ConversationService
}

// NewConversationHandler creates a new Conversation handler.
func NewConversationHandler(service interfaces.ConversationService) *ConversationHandler {
	return &ConversationHandler{service: service}
}

// ConversationCreated is the payload of a successful POST /conversations: the
// new Conversation's id and created_at. Its turns are empty, so the answer
// does not carry the list yet.
type ConversationCreated struct {
	ID        uuid.UUID `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateConversation answers POST /conversations: a new Conversation owned by
// the caller, with no body or an empty JSON object. A follow-up question is
// then asked with its id as POST /answer's conversation_id, which carries the
// last turns into retrieval and records the new turn.
//
//	@Summary		Create a Conversation
//	@Description	Creates a Conversation owned by the caller for a series of follow-up questions. The body is empty or an empty JSON object. Ask a question in it with POST /answer and its conversation_id; the answer carries the last five turns into retrieval and records the new turn.
//	@Tags			conversations
//	@Accept			json
//	@Produce		json
//	@Security		bearerAuth
//	@Success		201	{object}	handler.DataResponse{data=handler.ConversationCreated}
//	@Failure		400	{object}	handler.ErrorResponse	"A body that is not an empty JSON object"
//	@Failure		401	{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		413	{object}	handler.ErrorResponse	"Request body larger than 64 KiB"
//	@Failure		500	{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Router			/conversations [post]
func (h *ConversationHandler) CreateConversation(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
	if appErr := decodeEmptyObject(r); appErr != nil {
		writeError(w, r, appErr)
		return
	}
	conversation, err := h.service.CreateConversation(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(r.Context(), w, http.StatusCreated, ConversationCreated{
		ID:        conversation.ID,
		CreatedAt: conversation.CreatedAt,
	})
}

// GetConversation answers GET /conversations/{id} with the caller's
// Conversation and its turns in Seq order. An unknown id, another Principal's
// Conversation and a malformed id answer the same 404 body, so nothing about
// a foreign Conversation leaks.
//
//	@Summary		Read a Conversation and its turns
//	@Description	Returns the caller's Conversation with its turns in seq order: each turn's seq, raw question, rewritten_query when a query rewrite ran, cleaned answer and created_at. turns is [] when the Conversation has no turns, never null. A turn whose source Documents are no longer all readable to the caller keeps its seq, question and created_at, answers answer:"" with withheld:true, and drops rewritten_query; source_document_ids is never exposed. A Conversation the caller does not own, an unknown id and a malformed id all answer the same 404 body.
//	@Tags			conversations
//	@Produce		json
//	@Security		bearerAuth
//	@Param			id	path		string	true	"Conversation ID"
//	@Success		200	{object}	handler.DataResponse{data=types.Conversation}
//	@Failure		401	{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		404	{object}	handler.ErrorResponse	"No Conversation with that id belongs to the caller; a malformed id answers the same body"
//	@Failure		500	{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Router			/conversations/{id} [get]
func (h *ConversationHandler) GetConversation(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		// An id that is not a UUID names no Conversation. The zero UUID
		// reaches the service's one not-found error, so malformed, unknown and
		// foreign ids answer the same body.
		id = uuid.Nil
	}
	conversation, err := h.service.GetConversation(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(r.Context(), w, http.StatusOK, conversation)
}

// decodeEmptyObject accepts an empty body or one empty JSON object and
// rejects anything else, the shape POST /conversations documents.
func decodeEmptyObject(r *http.Request) *errors.AppError {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return requestBodyError(err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	req := *r
	req.Body = io.NopCloser(bytes.NewReader(body))
	_, appErr := decodeJSONRequest[struct{}](&req)
	return appErr
}
