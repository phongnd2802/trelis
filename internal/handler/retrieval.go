package handler

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"strings"

	"github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// retrievalBodyLimit bounds a POST /retrieve body. The request is a query
// and a small config, so anything larger is a client mistake worth refusing
// before it is decoded.
const retrievalBodyLimit = 64 << 10

// unknownJSONFieldPrefix is how encoding/json reports a field the struct does
// not declare when DisallowUnknownFields is set.
const unknownJSONFieldPrefix = `json: unknown field "`

// RetrievalHandler processes HTTP requests that search the Documents the
// caller can read.
type RetrievalHandler struct {
	service interfaces.RetrievalService
}

// NewRetrievalHandler creates a new retrieval handler.
func NewRetrievalHandler(service interfaces.RetrievalService) *RetrievalHandler {
	return &RetrievalHandler{service: service}
}

// Retrieve answers POST /retrieve with the ranked Chunks of the Documents the
// caller can read, the score and rank of every Retrieval Technique that found
// them and a trace of the steps. Several techniques are fused with Reciprocal
// Rank Fusion. A caller who can read nothing gets an empty result, never a
// 404, so the answer reveals nothing about what exists.
//
//	@Summary		Retrieve Chunks for a query
//	@Description	Runs every candidate Retrieval Technique named in retrieval_config (dense and/or bm25) over the Documents the caller can read. One technique answers in its own order with its own score; several run concurrently over max(top_k, 50) candidates each and are fused with Reciprocal Rank Fusion, score = the sum of 1/(rrf_k + rank) over the lists that carry the Chunk, then cut to top_k with equal fused scores ordered by chunk_id. rrf_k defaults to 60 and must be between 1 and 1000; it has no effect with one technique. Naming rerank adds a second stage: the candidate pool (the single list or the fused list, cut to rerank_candidates) is scored once by the rerank model, ordered by that score with ties by chunk_id, then cut to top_k; rerank cannot run alone. rerank_candidates defaults to 50, must be between 1 and 100 and at least top_k, and has no effect without rerank. Every result carries the score and 1-based rank of each technique whose list carried it, plus the rerank score and rank when it ran. A bm25 request needs no embedding model. A caller who can read nothing gets 200 with an empty results list. Malformed JSON, unknown fields, invalid values, an unknown or duplicate technique, rerank on its own and an out-of-range rrf_k or rerank_candidates are 400; a body over 64 KiB is 413; without models.embedding, a dense request is 503, and without models.rerank a request naming rerank is 503.
//	@Tags			retrieval
//	@Accept			json
//	@Produce		json
//	@Security		bearerAuth
//	@Param			request	body		types.RetrievalRequest	true	"The query, how many Chunks to return, which Retrieval Techniques to run, the RRF constant and the rerank candidate pool"
//	@Success		200		{object}	handler.DataResponse{data=types.RetrievalResult}
//	@Failure		400		{object}	handler.ErrorResponse	"Body that is not one JSON object, an unknown field or wrong value type, a blank or too-long query, an out-of-range top_k or an invalid retrieval_config (an unknown or duplicate technique, rerank on its own, an rrf_k outside 1..1000, or a rerank_candidates outside 1..100 or below top_k)"
//	@Failure		401		{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		413		{object}	handler.ErrorResponse	"Request body larger than 64 KiB"
//	@Failure		500		{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Failure		503		{object}	handler.ErrorResponse	"models.embedding is not configured so dense cannot run, or models.rerank is not configured so rerank cannot run (bm25 alone still works)"
//	@Router			/retrieve [post]
func (h *RetrievalHandler) Retrieve(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, retrievalBodyLimit)
	req, appErr := decodeRetrievalRequest(r)
	if appErr != nil {
		writeError(w, r, appErr)
		return
	}
	result, err := h.service.Retrieve(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(r.Context(), w, http.StatusOK, result)
}

// decodeRetrievalRequest decodes the one JSON object a POST /retrieve body
// must hold. A body that is not JSON, not an object, or holds more than one
// object is an ErrBadRequest; an unknown field or a field of the wrong type
// is a validation error, the same kind of client mistake as a bad
// retrieval_config.
func decodeRetrievalRequest(r *http.Request) (*types.RetrievalRequest, *errors.AppError) {
	dec := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, retrievalBodyError(err)
	}
	// The one-object check stays a bad request whatever the second value is.
	if err := dec.Decode(&struct{}{}); !stderrors.Is(err, io.EOF) {
		if isRequestBodyTooLarge(err) {
			return nil, errors.NewRequestEntityTooLargeError("Request body cannot exceed 64KB")
		}
		return nil, errors.NewBadRequestError("Invalid request body").
			WithDetails("the body must hold exactly one JSON object")
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.NewBadRequestError("Invalid request body").
			WithDetails("the body must be a JSON object")
	}

	var req types.RetrievalRequest
	obj := json.NewDecoder(bytes.NewReader(raw))
	// A typo in a config key must be an error, not a silently ignored field
	// that changes which techniques run.
	obj.DisallowUnknownFields()
	if err := obj.Decode(&req); err != nil {
		return nil, retrievalBodyError(err)
	}
	return &req, nil
}

// retrievalBodyError maps a decoding failure to the status its kind deserves.
func retrievalBodyError(err error) *errors.AppError {
	if isRequestBodyTooLarge(err) {
		return errors.NewRequestEntityTooLargeError("Request body cannot exceed 64KB")
	}
	if isRetrievalValidationError(err) {
		return errors.NewValidationError("Invalid request body").WithDetails(err.Error())
	}
	return errors.NewBadRequestError("Invalid request body").WithDetails(err.Error())
}

// isRetrievalValidationError reports whether err is a well-formed JSON body
// with a field or value the request does not accept.
func isRetrievalValidationError(err error) bool {
	if strings.HasPrefix(err.Error(), unknownJSONFieldPrefix) {
		return true
	}
	var typeErr *json.UnmarshalTypeError
	return stderrors.As(err, &typeErr)
}
