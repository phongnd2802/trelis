package service

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// techniqueRunner runs one technique over the readable Documents and returns
// its matches and the trace steps it took.
type techniqueRunner func(ctx context.Context, documentIDs []uuid.UUID, query string, depth int) ([]*types.ChunkMatch, []types.TraceStep, error)

// retrievalService answers a query with ranked Chunks. It resolves the
// caller's readable Documents first and passes that set into every search, so
// a Restricted Document is never returned or counted (ADR-0004). BM25's
// corpus statistics are shared across the whole index, so a Restricted
// Document can still shift a readable Chunk's score and rank (ADR-0009).
type retrievalService struct {
	documents interfaces.DocumentRepository
	chunks    interfaces.ChunkSearcher
	// embedder is nil when models.embedding is not configured, which makes
	// dense retrieval unavailable rather than silently returning nothing. BM25
	// needs no embedding model.
	embedder interfaces.Embedder
	// reranker is nil when models.rerank is not configured, which makes a
	// request naming rerank unavailable rather than silently skipping it.
	reranker interfaces.Reranker
	// runners maps every candidate Retrieval Technique this build implements
	// to the function that runs it. It is built once, so no request has to map
	// names to implementations again. Rerank has no runner: it reorders the
	// candidates the others found.
	runners map[types.RetrievalTechnique]techniqueRunner
}

// NewRetrievalService returns the retrieval use case. embedder may be nil
// when no embedding model is configured; dense retrieval then answers a
// service-unavailable error, while BM25 still runs. reranker may be nil when
// no rerank model is configured; a request naming rerank then answers a
// service-unavailable error.
func NewRetrievalService(documents interfaces.DocumentRepository, chunks interfaces.ChunkSearcher,
	embedder interfaces.Embedder, reranker interfaces.Reranker,
) interfaces.RetrievalService {
	s := &retrievalService{documents: documents, chunks: chunks, embedder: embedder, reranker: reranker}
	s.runners = map[types.RetrievalTechnique]techniqueRunner{
		types.RetrievalTechniqueDense: s.runDense,
		types.RetrievalTechniqueBM25:  s.runBM25,
	}
	return s
}

// Retrieve validates req, resolves the caller's readable Documents and runs
// every candidate Retrieval Technique req names over them. One technique
// answers with its own score and rank; several run concurrently and their
// ranks are fused with Reciprocal Rank Fusion. When the config also names
// rerank, the candidate pool (fused when there are several) is reordered by
// the rerank model. When it names parent_child, the ordered pool is replaced
// by the Parent Chunks its Chunks link to, siblings collapse into the best
// child's position, and only then is the answer cut to top_k. Reads only, so
// no transaction is needed: the readable set is resolved on every request,
// never cached. A Principal who can read nothing still gets the trace of the
// steps that ran, with empty results, so the answer has the same shape as one
// that matched nothing.
func (s *retrievalService) Retrieve(ctx context.Context, req *types.RetrievalRequest) (*types.RetrievalResult, error) {
	principal, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, apperrors.NewValidationError(err.Error())
	}
	techniques := req.RetrievalConfig.Techniques
	if slices.Contains(techniques, types.RetrievalTechniqueDense) && s.embedder == nil {
		return nil, apperrors.NewServiceUnavailableError(
			"dense retrieval needs models.embedding to be configured")
	}
	candidateTechniques := req.RetrievalConfig.CandidateTechniques()
	rerankOn := req.RetrievalConfig.Reranks()
	parentChildOn := req.RetrievalConfig.ExpandsParents()
	if rerankOn && s.reranker == nil {
		return nil, apperrors.NewServiceUnavailableError(
			"rerank needs models.rerank to be configured")
	}
	// Non-nil slices keep an empty answer as "results": [] rather than null.
	result := &types.RetrievalResult{
		Results: []types.RetrievedChunk{},
		Trace:   make([]types.TraceStep, 0, len(candidateTechniques)*2+3),
	}

	documentIDs, err := s.documents.ListReadableDocumentIDs(ctx, principal.ID)
	if err != nil {
		return nil, fmt.Errorf("list readable documents: %w", err)
	}
	// One technique fetches exactly top_k, as it always has. Fusion needs a
	// deeper pool per list so a Chunk that every list places low can still
	// reach the fused top_k. Parent-child needs the same depth without rerank,
	// because collapsing siblings can empty as many places as it merges.
	// Rerank needs a pool of its own size to score before the cut.
	poolSize := *req.TopK
	depth := *req.TopK
	if len(candidateTechniques) > 1 {
		depth = max(depth, types.RetrievalFusionCandidates)
	}
	if parentChildOn && !rerankOn {
		depth = max(depth, types.RetrievalFusionCandidates)
		poolSize = max(poolSize, types.RetrievalFusionCandidates)
	}
	if rerankOn {
		poolSize = *req.RetrievalConfig.RerankCandidates
		depth = max(depth, poolSize)
	}
	runs, err := s.runTechniques(ctx, candidateTechniques, documentIDs, req.Query, depth)
	if err != nil {
		return nil, err
	}

	lists := make(map[types.RetrievalTechnique][]*types.ChunkMatch, len(runs))
	for _, run := range runs {
		// A technique keeps its own steps; request order, not completion
		// order, so the trace is deterministic.
		result.Trace = append(result.Trace, run.steps...)
		lists[run.technique] = run.matches
	}

	var pool []fusedChunk
	if len(runs) == 1 {
		pool = singleTechniqueChunks(runs[0].technique, runs[0].matches)
		if rerankOn && len(pool) > poolSize {
			pool = pool[:poolSize]
		}
	} else {
		start := time.Now()
		var distinct int
		pool, distinct = fuseRRF(lists, *req.RetrievalConfig.RRFK, poolSize)
		result.Trace = append(result.Trace, types.TraceStep{
			Step:       types.TraceStepFuse,
			Technique:  types.RetrievalTechniqueRRF,
			DurationMS: time.Since(start).Milliseconds(),
			Candidates: &distinct,
		})
	}
	if rerankOn {
		reranked, step, err := s.rerank(ctx, req.Query, pool)
		if err != nil {
			return nil, err
		}
		result.Trace = append(result.Trace, step)
		pool = reranked
	}
	if !parentChildOn {
		// Rerank returns its whole sorted pool, so the cut to top_k lives
		// here when there is no expansion to do first.
		if len(pool) > *req.TopK {
			pool = pool[:*req.TopK]
		}
		result.Results = retrievedChunks(pool)
		return result, nil
	}

	expanded, step, err := s.expandParents(ctx, documentIDs, pool, *req.TopK)
	if err != nil {
		return nil, err
	}
	result.Trace = append(result.Trace, step)
	result.Results = expandedResults(expanded)
	return result, nil
}

// techniqueRun is one technique's matches and trace steps, kept in request
// order so the trace is deterministic even though the techniques run
// concurrently.
type techniqueRun struct {
	technique types.RetrievalTechnique
	matches   []*types.ChunkMatch
	steps     []types.TraceStep
}

// runTechniques runs every technique concurrently over the same readable set
// and the same candidate depth. Any technique's error fails the request.
func (s *retrievalService) runTechniques(ctx context.Context, techniques []types.RetrievalTechnique,
	documentIDs []uuid.UUID, query string, depth int,
) ([]techniqueRun, error) {
	runs := make([]techniqueRun, len(techniques))
	runners := make([]techniqueRunner, len(techniques))
	// Resolve every runner before starting a goroutine, so an unknown
	// technique fails without leaving goroutines writing to a slice nobody
	// waits for.
	for i, technique := range techniques {
		run, ok := s.runners[technique]
		if !ok {
			return nil, fmt.Errorf("no runner for retrieval technique %q", technique)
		}
		runs[i].technique = technique
		runners[i] = run
	}
	group, groupCtx := errgroup.WithContext(ctx)
	for i := range techniques {
		group.Go(func() error {
			matches, steps, err := runners[i](groupCtx, documentIDs, query, depth)
			if err != nil {
				return err
			}
			runs[i].matches, runs[i].steps = matches, steps
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return runs, nil
}

// runDense embeds the query and ranks the readable Chunks by cosine
// similarity, returning the embed and search steps.
func (s *retrievalService) runDense(ctx context.Context, documentIDs []uuid.UUID, query string,
	depth int,
) ([]*types.ChunkMatch, []types.TraceStep, error) {
	model := s.embedder.GetModelName()
	start := time.Now()
	vectors, err := s.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vectors) != 1 {
		return nil, nil, fmt.Errorf("embedding model returned %d vectors for one query", len(vectors))
	}
	steps := []types.TraceStep{{
		Step:       types.TraceStepEmbedQuery,
		Technique:  types.RetrievalTechniqueDense,
		DurationMS: time.Since(start).Milliseconds(),
		Model:      model,
	}}

	start = time.Now()
	matches, err := s.chunks.SearchDense(ctx, documentIDs, vectors[0], model, depth)
	if err != nil {
		return nil, nil, fmt.Errorf("search dense chunks: %w", err)
	}
	candidates := len(matches)
	steps = append(steps, types.TraceStep{
		Step:       types.TraceStepSearch,
		Technique:  types.RetrievalTechniqueDense,
		DurationMS: time.Since(start).Milliseconds(),
		Candidates: &candidates,
	})
	return matches, steps, nil
}

// runBM25 ranks the readable Chunks by pg_search's BM25 score, with no
// embedding call, and returns the search step.
func (s *retrievalService) runBM25(ctx context.Context, documentIDs []uuid.UUID, query string,
	depth int,
) ([]*types.ChunkMatch, []types.TraceStep, error) {
	start := time.Now()
	matches, err := s.chunks.SearchBM25(ctx, documentIDs, query, depth)
	if err != nil {
		return nil, nil, fmt.Errorf("search bm25 chunks: %w", err)
	}
	candidates := len(matches)
	return matches, []types.TraceStep{{
		Step:       types.TraceStepSearch,
		Technique:  types.RetrievalTechniqueBM25,
		DurationMS: time.Since(start).Milliseconds(),
		Candidates: &candidates,
	}}, nil
}

// fusedChunk is one Chunk after fusion: the Chunk, its Parent Chunk when it
// has one, the score that orders it, and the score and 1-based rank every
// list that carried it gave it.
type fusedChunk struct {
	chunk    *types.Chunk
	parentID *uuid.UUID
	score    float64
	scores   map[types.RetrievalTechnique]float64
	ranks    map[types.RetrievalTechnique]int
}

// singleTechniqueChunks turns one technique's matches into fusedChunks in the
// order the repository returned them, each with that technique's own score and
// 1-based rank. A one-technique answer therefore keeps its score and order
// while sharing the result shape fusion uses.
func singleTechniqueChunks(technique types.RetrievalTechnique, matches []*types.ChunkMatch) []fusedChunk {
	out := make([]fusedChunk, 0, len(matches))
	for i, match := range matches {
		out = append(out, fusedChunk{
			chunk:    match.Chunk,
			parentID: match.ParentID,
			score:    match.Score,
			scores:   map[types.RetrievalTechnique]float64{technique: match.Score},
			ranks:    map[types.RetrievalTechnique]int{technique: i + 1},
		})
	}
	return out
}

// retrievedChunk renders one fused entry as a result.
func retrievedChunk(entry fusedChunk) types.RetrievedChunk {
	return types.RetrievedChunk{
		ChunkID:       entry.chunk.ID,
		DocumentID:    entry.chunk.DocumentID,
		Seq:           entry.chunk.Seq,
		Content:       entry.chunk.Content,
		ContextHeader: entry.chunk.ContextHeader,
		Score:         entry.score,
		Scores:        entry.scores,
		Ranks:         entry.ranks,
	}
}

// retrievedChunks renders fusedChunks as the answer's results, in their
// current order.
func retrievedChunks(chunks []fusedChunk) []types.RetrievedChunk {
	out := make([]types.RetrievedChunk, 0, len(chunks))
	for _, entry := range chunks {
		out = append(out, retrievedChunk(entry))
	}
	return out
}

// parentExpansion is one parent_child result: the best matching child, the
// Parent Chunk that supplies the content (nil when the child stands alone),
// and every child that collapsed into it in rank order.
type parentExpansion struct {
	entry   fusedChunk
	parent  *types.ParentChunk
	matched []uuid.UUID
}

// expandParents replaces each Chunk in pool with its Parent Chunk, collapses
// several Chunks of one Parent Chunk into the first (best) one's position, and
// cuts the collapsed list to topK. A Chunk with no Parent Chunk, or one whose
// Parent Chunk is no longer there, stands alone. The returned step names the
// children behind every returned result; its candidate count is the results
// before the cut, so a caller can see what the cut removed.
func (s *retrievalService) expandParents(ctx context.Context, documentIDs []uuid.UUID,
	pool []fusedChunk, topK int,
) ([]parentExpansion, types.TraceStep, error) {
	start := time.Now()
	parents := make(map[uuid.UUID]*types.ParentChunk)
	// A fallback-only pool has nothing to look up, so it costs no query.
	if parentIDs := distinctParentIDs(pool); len(parentIDs) > 0 {
		found, err := s.chunks.ListParentChunks(ctx, documentIDs, parentIDs)
		if err != nil {
			return nil, types.TraceStep{}, fmt.Errorf("list parent chunks: %w", err)
		}
		for _, parent := range found {
			parents[parent.ID] = parent
		}
	}
	expanded := collapseParents(pool, parents)
	candidates := len(expanded)
	if len(expanded) > topK {
		expanded = expanded[:topK]
	}
	expandedTrace := parentExpansions(expanded)
	step := types.TraceStep{
		Step:       types.TraceStepExpandParents,
		Technique:  types.RetrievalTechniqueParentChild,
		DurationMS: time.Since(start).Milliseconds(),
		Candidates: &candidates,
		Expanded:   &expandedTrace,
	}
	return expanded, step, nil
}

// distinctParentIDs returns the Parent Chunk IDs pool's Chunks link to, in
// first-match order, without duplicates.
func distinctParentIDs(pool []fusedChunk) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(pool))
	ids := make([]uuid.UUID, 0, len(pool))
	for _, entry := range pool {
		if entry.parentID == nil {
			continue
		}
		if _, ok := seen[*entry.parentID]; ok {
			continue
		}
		seen[*entry.parentID] = struct{}{}
		ids = append(ids, *entry.parentID)
	}
	return ids
}

// collapseParents walks pool in rank order and keeps one expansion per Parent
// Chunk at its best child's position; a child with no parent, or whose parent
// is missing from parents, stands alone.
func collapseParents(pool []fusedChunk, parents map[uuid.UUID]*types.ParentChunk) []parentExpansion {
	expanded := make([]parentExpansion, 0, len(pool))
	position := make(map[uuid.UUID]int, len(pool))
	for _, entry := range pool {
		var parent *types.ParentChunk
		if entry.parentID != nil {
			parent = parents[*entry.parentID]
		}
		if parent != nil {
			if i, ok := position[parent.ID]; ok {
				expanded[i].matched = append(expanded[i].matched, entry.chunk.ID)
				continue
			}
			position[parent.ID] = len(expanded)
		}
		expanded = append(expanded, parentExpansion{
			entry:   entry,
			parent:  parent,
			matched: []uuid.UUID{entry.chunk.ID},
		})
	}
	return expanded
}

// expandedResults renders parent_child expansions as the answer's results:
// ids, score, scores and ranks from the best-ranked matched Chunk, content
// and context header from the Parent Chunk when there is one.
func expandedResults(expanded []parentExpansion) []types.RetrievedChunk {
	out := make([]types.RetrievedChunk, 0, len(expanded))
	for _, expansion := range expanded {
		result := retrievedChunk(expansion.entry)
		if expansion.parent != nil {
			result.Content = expansion.parent.Content
			result.ContextHeader = expansion.parent.ContextHeader
			parentID := expansion.parent.ID
			result.ParentChunkID = &parentID
		}
		out = append(out, result)
	}
	return out
}

// parentExpansions renders the trace entries for expansions, one per result
// in result order.
func parentExpansions(expanded []parentExpansion) []types.ParentExpansion {
	out := make([]types.ParentExpansion, 0, len(expanded))
	for _, expansion := range expanded {
		entry := types.ParentExpansion{
			ChunkID:         expansion.entry.chunk.ID,
			MatchedChunkIDs: expansion.matched,
		}
		if expansion.parent != nil {
			parentID := expansion.parent.ID
			entry.ParentChunkID = &parentID
		}
		out = append(out, entry)
	}
	return out
}

// rerank orders the candidate pool by the rerank model's score, adds that
// score and 1-based rank beside the first stage's, and returns the whole
// sorted pool. The caller cuts to top_k, after parent-child expansion when
// that stage runs, so collapsing siblings still sees every scored Chunk. Ties
// order by Chunk ID, the same way the other stages break them. An empty pool
// makes no model call but still records the step, so the answer keeps its
// shape.
func (s *retrievalService) rerank(ctx context.Context, query string,
	pool []fusedChunk,
) ([]fusedChunk, types.TraceStep, error) {
	poolSize := len(pool)
	step := types.TraceStep{
		Step:       types.TraceStepRerank,
		Technique:  types.RetrievalTechniqueRerank,
		Candidates: &poolSize,
	}
	if poolSize == 0 {
		return pool, step, nil
	}
	texts := make([]string, poolSize)
	for i, entry := range pool {
		texts[i] = chunkText(entry.chunk)
	}
	model := s.reranker.GetModelName()
	start := time.Now()
	scores, err := s.reranker.Rerank(ctx, query, texts)
	step.DurationMS = time.Since(start).Milliseconds()
	step.Model = model
	if err != nil {
		return nil, types.TraceStep{}, fmt.Errorf("rerank candidates: %w", err)
	}
	if len(scores) != poolSize {
		return nil, types.TraceStep{}, fmt.Errorf("rerank model returned %d scores for %d candidates", len(scores), poolSize)
	}
	for i := range pool {
		pool[i].score = scores[i]
		pool[i].scores[types.RetrievalTechniqueRerank] = scores[i]
	}
	slices.SortFunc(pool, func(a, b fusedChunk) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return bytes.Compare(a.chunk.ID[:], b.chunk.ID[:])
	})
	for i := range pool {
		pool[i].ranks[types.RetrievalTechniqueRerank] = i + 1
	}
	return pool, step, nil
}

// fuseRRF combines technique lists with Reciprocal Rank Fusion: a Chunk's
// score is the sum over the lists that carry it of 1/(k+rank), with 1-based
// ranks, so a Chunk missing from a list gets nothing from it. It returns the
// fused list cut to topK and the number of distinct Chunks fused before the
// cut. The order is fused score descending, then Chunk ID ascending, so equal
// scores keep a deterministic order.
func fuseRRF(lists map[types.RetrievalTechnique][]*types.ChunkMatch, k, topK int) ([]fusedChunk, int) {
	techniques := make([]types.RetrievalTechnique, 0, len(lists))
	for technique := range lists {
		techniques = append(techniques, technique)
	}
	slices.Sort(techniques)

	byChunk := make(map[uuid.UUID]*fusedChunk)
	for _, technique := range techniques {
		for i, match := range lists[technique] {
			entry, ok := byChunk[match.Chunk.ID]
			if !ok {
				entry = &fusedChunk{
					chunk:    match.Chunk,
					parentID: match.ParentID,
					scores:   make(map[types.RetrievalTechnique]float64, len(lists)),
					ranks:    make(map[types.RetrievalTechnique]int, len(lists)),
				}
				byChunk[match.Chunk.ID] = entry
			}
			entry.scores[technique] = match.Score
			entry.ranks[technique] = i + 1
			entry.score += 1 / float64(k+i+1)
		}
	}

	distinct := len(byChunk)
	out := make([]fusedChunk, 0, distinct)
	for _, entry := range byChunk {
		out = append(out, *entry)
	}
	slices.SortFunc(out, func(a, b fusedChunk) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return bytes.Compare(a.chunk.ID[:], b.chunk.ID[:])
	})
	if len(out) > topK {
		out = out[:topK]
	}
	return out, distinct
}
