package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// retrievalService answers a query with ranked Chunks. It resolves the
// caller's readable Documents first and passes that set into the search, so a
// Restricted Document is never returned or counted (ADR-0004). BM25's corpus
// statistics are shared across the whole index, so a Restricted Document can
// still shift a readable Chunk's score and rank (ADR-0009).
type retrievalService struct {
	documents interfaces.DocumentRepository
	chunks    interfaces.ChunkSearcher
	// embedder is nil when models.embedding is not configured, which makes
	// dense retrieval unavailable rather than silently returning nothing. BM25
	// needs no embedding model.
	embedder interfaces.Embedder
}

// NewRetrievalService returns the retrieval use case. embedder may be nil
// when no embedding model is configured; dense retrieval then answers a
// service-unavailable error, while BM25 still runs.
func NewRetrievalService(documents interfaces.DocumentRepository, chunks interfaces.ChunkSearcher,
	embedder interfaces.Embedder,
) interfaces.RetrievalService {
	return &retrievalService{documents: documents, chunks: chunks, embedder: embedder}
}

// Retrieve validates req, resolves the caller's readable Documents and runs
// the one Retrieval Technique req names over them. Reads only, so no
// transaction is needed: the readable set is resolved on every request, never
// cached. A Principal who can read nothing still gets the trace of the steps
// that ran, with empty results, so the answer has the same shape as one that
// matched nothing.
func (s *retrievalService) Retrieve(ctx context.Context, req *types.RetrievalRequest) (*types.RetrievalResult, error) {
	principal, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, apperrors.NewValidationError(err.Error())
	}
	// Validate admits exactly one known technique, so this is the one to run.
	technique := req.RetrievalConfig.Techniques[0]
	if technique == types.RetrievalTechniqueDense && s.embedder == nil {
		return nil, apperrors.NewServiceUnavailableError(
			"dense retrieval needs models.embedding to be configured")
	}
	// Non-nil slices keep an empty answer as "results": [] rather than null.
	result := &types.RetrievalResult{
		Results: []types.RetrievedChunk{},
		Trace:   make([]types.TraceStep, 0, 2),
	}

	documentIDs, err := s.documents.ListReadableDocumentIDs(ctx, principal.ID)
	if err != nil {
		return nil, fmt.Errorf("list readable documents: %w", err)
	}
	if technique == types.RetrievalTechniqueBM25 {
		return s.retrieveBM25(ctx, result, documentIDs, req)
	}
	return s.retrieveDense(ctx, result, documentIDs, req)
}

// retrieveDense embeds the query and ranks the readable Chunks by cosine
// similarity, appending the embed and search steps to result's trace.
func (s *retrievalService) retrieveDense(ctx context.Context, result *types.RetrievalResult,
	documentIDs []uuid.UUID, req *types.RetrievalRequest,
) (*types.RetrievalResult, error) {
	model := s.embedder.GetModelName()
	start := time.Now()
	vectors, err := s.embedder.Embed(ctx, []string{req.Query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("embedding model returned %d vectors for one query", len(vectors))
	}
	result.Trace = append(result.Trace, types.TraceStep{
		Step:       types.TraceStepEmbedQuery,
		Technique:  types.RetrievalTechniqueDense,
		DurationMS: time.Since(start).Milliseconds(),
		Model:      model,
	})

	start = time.Now()
	matches, err := s.chunks.SearchDense(ctx, documentIDs, vectors[0], model, *req.TopK)
	if err != nil {
		return nil, fmt.Errorf("search dense chunks: %w", err)
	}
	candidates := len(matches)
	result.Trace = append(result.Trace, types.TraceStep{
		Step:       types.TraceStepSearch,
		Technique:  types.RetrievalTechniqueDense,
		DurationMS: time.Since(start).Milliseconds(),
		Candidates: &candidates,
	})
	return appendMatches(result, types.RetrievalTechniqueDense, matches), nil
}

// retrieveBM25 ranks the readable Chunks by pg_search's BM25 score, with no
// embedding call, and appends the search step to result's trace.
func (s *retrievalService) retrieveBM25(ctx context.Context, result *types.RetrievalResult,
	documentIDs []uuid.UUID, req *types.RetrievalRequest,
) (*types.RetrievalResult, error) {
	start := time.Now()
	matches, err := s.chunks.SearchBM25(ctx, documentIDs, req.Query, *req.TopK)
	if err != nil {
		return nil, fmt.Errorf("search bm25 chunks: %w", err)
	}
	candidates := len(matches)
	result.Trace = append(result.Trace, types.TraceStep{
		Step:       types.TraceStepSearch,
		Technique:  types.RetrievalTechniqueBM25,
		DurationMS: time.Since(start).Milliseconds(),
		Candidates: &candidates,
	})
	return appendMatches(result, types.RetrievalTechniqueBM25, matches), nil
}

// appendMatches adds matches to result in the order the repository returned
// them, each with the score of the one technique that produced it. #34's RRF
// adds a combined entry to Scores; one technique needs only its own.
func appendMatches(result *types.RetrievalResult, technique types.RetrievalTechnique,
	matches []*types.ChunkMatch,
) *types.RetrievalResult {
	for _, match := range matches {
		result.Results = append(result.Results, types.RetrievedChunk{
			ChunkID:       match.Chunk.ID,
			DocumentID:    match.Chunk.DocumentID,
			Seq:           match.Chunk.Seq,
			Content:       match.Chunk.Content,
			ContextHeader: match.Chunk.ContextHeader,
			Score:         match.Score,
			Scores:        map[types.RetrievalTechnique]float64{technique: match.Score},
		})
	}
	return result
}
