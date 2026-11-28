package handler

import (
	"net/http"

	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

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
//	@Description	Runs every candidate Retrieval Technique named in retrieval_config (dense and/or bm25) over the Documents the caller can read. One technique answers in its own order with its own score; several run concurrently over max(top_k, 50) candidates each and are fused with Reciprocal Rank Fusion, score = the sum of 1/(rrf_k + rank) over the lists that carry the Chunk, then cut to top_k with equal fused scores ordered by chunk_id. rrf_k defaults to 60 and must be between 1 and 1000; it has no effect with one technique. Naming rerank adds a second stage: the candidate pool (the single list or the fused list, cut to rerank_candidates) is scored once by the rerank model, ordered by that score with ties by chunk_id; rerank cannot run alone. rerank_candidates defaults to 50, must be between 1 and 100 and at least top_k, and has no effect without rerank. Naming parent_child adds a final stage: each matched Chunk is replaced by its Parent Chunk, several matched Chunks of one Parent Chunk collapse into the position of the best-ranked matched Chunk, and a Chunk with no Parent Chunk stands alone; chunk_id, document_id, seq, score, scores and ranks stay the best-ranked matched Chunk's, content and context_header become the Parent Chunk's, and parent_chunk_id names it (absent for a standalone Chunk). Without rerank parent_child searches max(top_k, 50) deep; with rerank the pool stays rerank_candidates and the cut to top_k happens after the collapse. Naming query_rewrite adds a first stage: when the request carries Conversation History, which POST /answer fills, the chat role rewrites the latest question into one standalone query and every technique, and rerank, searches with it; the trace then starts with rewrite_query carrying rewritten_query and the call's usage. Without history no model call is made, so POST /retrieve accepts query_rewrite and never calls a model. query_rewrite cannot run alone. Naming query_decomposition adds a first stage that needs no history: the chat role splits the query into 2 to 4 self-contained sub-questions and every candidate technique runs for the original and each sub-question, all of their lists fused with one Reciprocal Rank Fusion; the trace's decompose_query step carries queries with each query's own chunk_ids and every embed_query and search step carries query_index, rerank still scores against the original query, and fewer than 2 usable sub-questions runs only the original. query_decomposition cannot run alone. Every result carries the score and 1-based rank of each technique whose list carried it, plus the rerank score and rank when it ran. A bm25 request needs no embedding model. A caller who can read nothing gets 200 with an empty results list. Malformed JSON, unknown fields, invalid values, an unknown or duplicate technique, a config whose only techniques are stages (query_rewrite, query_decomposition, rerank, parent_child), an out-of-range rrf_k or rerank_candidates are 400; a body over 64 KiB is 413; without models.embedding, a dense request is 503, without models.rerank a request naming rerank is 503, and without models.chat a request naming query_rewrite or query_decomposition is 503. A model provider's rejection of the call (its credentials, credit, permission or rate limit) is 502 and names the model role in its message; any other model failure is a generic 500.
//	@Tags			retrieval
//	@Accept			json
//	@Produce		json
//	@Security		bearerAuth
//	@Param			request	body		types.RetrievalRequest	true	"The query, how many Chunks to return, which Retrieval Techniques to run (dense, bm25, query_rewrite, query_decomposition, rerank, parent_child), the RRF constant and the rerank candidate pool"
//	@Success		200		{object}	handler.DataResponse{data=types.RetrievalResult}
//	@Failure		400		{object}	handler.ErrorResponse	"Body that is not one JSON object, an unknown field or wrong value type, a blank or too-long query, an out-of-range top_k or an invalid retrieval_config (an unknown or duplicate technique, only stage techniques such as query_rewrite, query_decomposition, rerank or parent_child, an rrf_k outside 1..1000, or a rerank_candidates outside 1..100 or below top_k)"
//	@Failure		401		{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		413		{object}	handler.ErrorResponse	"Request body larger than 64 KiB"
//	@Failure		500		{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Failure		502		{object}	handler.ErrorResponse	"A model provider rejected the call because of its credentials, credit, permission or rate limit (an embedding or rerank status 401, 402, 403 or 429); the message names the model role, never the provider's body"
//	@Failure		503		{object}	handler.ErrorResponse	"models.embedding is not configured so dense cannot run, models.rerank is not configured so rerank cannot run, or models.chat is not configured so query_rewrite or query_decomposition cannot run (bm25 alone still works)"
//	@Router			/retrieve [post]
func (h *RetrievalHandler) Retrieve(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
	req, appErr := decodeJSONRequest[types.RetrievalRequest](r)
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
