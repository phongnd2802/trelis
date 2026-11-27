package service

import (
	"context"
	"fmt"
	"time"

	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// retrievalService answers a query with ranked Chunks. It resolves the
// caller's readable Documents first and passes that set into the search, so a
// Restricted Document can neither be found nor influence a count (ADR-0004).
type retrievalService struct {
	documents interfaces.DocumentRepository
	chunks    interfaces.ChunkSearcher
	// embedder is nil when models.embedding is not configured, which makes
	// dense retrieval unavailable rather than silently returning nothing.
	embedder interfaces.Embedder
}

// NewRetrievalService returns the retrieval use case. embedder may be nil
// when no embedding model is configured; Retrieve then answers a
// service-unavailable error.
func NewRetrievalService(documents interfaces.DocumentRepository, chunks interfaces.ChunkSearcher,
	embedder interfaces.Embedder,
) interfaces.RetrievalService {
	return &retrievalService{documents: documents, chunks: chunks, embedder: embedder}
}

// Retrieve validates req, resolves the caller's readable Documents and runs
// the dense search over them. Reads only, so no transaction is needed: the
// readable set is resolved on every request, never cached. A Principal who
// can read nothing still gets the trace of the steps that ran, with empty
// results, so the answer has the same shape as one that matched nothing.
func (s *retrievalService) Retrieve(ctx context.Context, req *types.RetrievalRequest) (*types.RetrievalResult, error) {
	principal, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, apperrors.NewValidationError(err.Error())
	}
	if s.embedder == nil {
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

	technique := types.RetrievalTechniqueDense
	for _, match := range matches {
		result.Results = append(result.Results, types.RetrievedChunk{
			ChunkID:       match.Chunk.ID,
			DocumentID:    match.Chunk.DocumentID,
			Seq:           match.Chunk.Seq,
			Content:       match.Chunk.Content,
			ContextHeader: match.Chunk.ContextHeader,
			Score:         match.Score,
			// #34's RRF adds its own entry here; dense alone has one.
			Scores: map[types.RetrievalTechnique]float64{technique: match.Score},
		})
	}
	// The SQL orders the matches by score descending, ties by Chunk ID, and
	// this loop keeps that order.
	return result, nil
}
