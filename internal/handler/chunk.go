package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// ChunkHandler processes HTTP requests that read one Chunk, so a citation can
// open its source.
type ChunkHandler struct {
	service interfaces.ChunkService
}

// NewChunkHandler creates a new Chunk handler.
func NewChunkHandler(service interfaces.ChunkService) *ChunkHandler {
	return &ChunkHandler{service: service}
}

// GetChunk answers GET /chunks/{id} with the Chunk when the caller can read
// its Document, or 404 when it does not exist or the caller cannot read it,
// so a Restricted Document's existence is not revealed.
//
//	@Summary		Get a Chunk
//	@Description	Returns the Chunk, without its embedding, when the caller can read its Document. A Chunk that does not exist and one whose Document the caller cannot read answer the same 404, so a Restricted Document's existence is not revealed.
//	@Tags			chunks
//	@Produce		json
//	@Security		bearerAuth
//	@Param			id	path	string	true	"Chunk ID"	format(uuid)
//	@Success		200	{object}	handler.DataResponse{data=types.Chunk}
//	@Failure		400	{object}	handler.ErrorResponse	"ID that is not a UUID"
//	@Failure		401	{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		404	{object}	handler.ErrorResponse	"No readable Chunk with that ID"
//	@Failure		500	{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Router			/chunks/{id} [get]
func (h *ChunkHandler) GetChunk(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, errors.NewBadRequestError("Invalid chunk ID"))
		return
	}
	chunk, err := h.service.GetChunkByID(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(r.Context(), w, http.StatusOK, chunk)
}
