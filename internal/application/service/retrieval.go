package service

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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
	// chat is nil when models.chat is not configured, which makes a request
	// naming query_rewrite or query_decomposition unavailable rather than
	// searching with the raw follow-up or skipping the split. Both stages
	// share the one role.
	chat interfaces.Generator
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
// service-unavailable error. chat may be nil when no chat model is
// configured; a request naming query_rewrite or query_decomposition then
// answers a service-unavailable error.
func NewRetrievalService(documents interfaces.DocumentRepository, chunks interfaces.ChunkSearcher,
	embedder interfaces.Embedder, reranker interfaces.Reranker, chat interfaces.Generator,
) interfaces.RetrievalService {
	s := &retrievalService{documents: documents, chunks: chunks, embedder: embedder, reranker: reranker,
		chat: chat}
	s.runners = map[types.RetrievalTechnique]techniqueRunner{
		types.RetrievalTechniqueDense: s.runDense,
		types.RetrievalTechniqueBM25:  s.runBM25,
	}
	return s
}

// Retrieve validates req, resolves the caller's readable Documents and runs
// every candidate Retrieval Technique req names over them. One technique
// answers with its own score and rank; several run concurrently and their
// ranks are fused with Reciprocal Rank Fusion. When the config names
// query_decomposition, the chat role first splits the query into
// sub-questions and every technique runs for the original and each
// sub-question, all of their lists fused into one. When the config also names
// rerank, the candidate pool (fused when there are several lists) is reordered
// by the rerank model against the original query. When it names parent_child,
// the ordered pool is replaced by the Parent Chunks its Chunks link to,
// several matched Chunks of one Parent Chunk collapse into the position of the
// best-ranked matched Chunk, and only then is the answer cut to top_k. Reads
// only, so no transaction is needed: the readable set is resolved on every
// request, never cached. A Principal who can read nothing still gets the trace
// of the steps that ran, with empty results, so the answer has the same shape
// as one that matched nothing.
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
	rewriteOn := req.RetrievalConfig.RewritesQuery()
	decomposeOn := req.RetrievalConfig.DecomposesQuery()
	rerankOn := req.RetrievalConfig.Reranks()
	parentChildOn := req.RetrievalConfig.ExpandsParents()
	if rewriteOn && s.chat == nil {
		return nil, apperrors.NewServiceUnavailableError(
			"query rewrite needs models.chat to be configured")
	}
	if decomposeOn && s.chat == nil {
		return nil, apperrors.NewServiceUnavailableError(
			"query decomposition needs models.chat to be configured")
	}
	if rerankOn && s.reranker == nil {
		return nil, apperrors.NewServiceUnavailableError(
			"rerank needs models.rerank to be configured")
	}
	// Non-nil slices keep an empty answer as "results": [] rather than null.
	result := &types.RetrievalResult{
		Results: []types.RetrievedChunk{},
		Trace:   make([]types.TraceStep, 0, len(candidateTechniques)*2+4),
	}

	// A follow-up with history and query_rewrite calls the chat role once,
	// before any technique runs, and every technique then searches with the
	// standalone query. Without history there is nothing to resolve, so no
	// call and no step: the raw question is already standalone.
	query := req.Query
	if rewriteOn && len(req.History) > 0 {
		rewritten, step, err := s.rewriteQuery(ctx, req.Query, req.History)
		if err != nil {
			return nil, err
		}
		recordChatStep(result, step)
		result.RewrittenQuery = &rewritten
		query = rewritten
	}
	result.Query = query

	// query_decomposition calls the chat role once, after the rewrite so it
	// splits the query that will actually be searched. It needs no history,
	// so POST /retrieve measures it too. Fewer than two usable sub-questions
	// leaves only the original, but the step still records the call.
	queries := []string{query}
	decomposeStep := -1
	if decomposeOn {
		subQuestions, step, err := s.decomposeQuery(ctx, query)
		if err != nil {
			return nil, err
		}
		decomposeStep = recordChatStep(result, step)
		if len(subQuestions) > 1 {
			queries = append(queries, subQuestions...)
		}
	}

	documentIDs, err := s.documents.ListReadableDocumentIDs(ctx, principal.ID)
	if err != nil {
		return nil, fmt.Errorf("list readable documents: %w", err)
	}
	// One list fetches exactly top_k, as it always has. Fusion needs a deeper
	// pool per list so a Chunk that every list places low can still reach the
	// fused top_k; decomposition makes one technique several lists, so the
	// rule follows the number of lists, not techniques. Parent-child needs the
	// same depth without rerank, because collapsing several matched Chunks of
	// one Parent Chunk can empty as many places as it merges. Rerank needs a
	// pool of its own size to score before the cut.
	poolSize := *req.TopK
	depth := *req.TopK
	if len(queries)*len(candidateTechniques) > 1 {
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
	runs, err := s.runTechniques(ctx, candidateTechniques, documentIDs, queries, depth)
	if err != nil {
		return nil, err
	}

	// One list per (query, technique) run, in query index then request order,
	// so both the trace and the k-th list of one technique are deterministic
	// even though the runs finish in any order. perQuery keeps each query's
	// own lists for its evidence; a decomposed run stamps every search step
	// with the query index it ran for.
	lists := make([]techniqueList, 0, len(runs))
	perQuery := make([][]techniqueList, len(queries))
	for i := range runs {
		run := &runs[i]
		if decomposeOn {
			queryIndex := run.queryIndex
			for j := range run.steps {
				run.steps[j].QueryIndex = &queryIndex
			}
		}
		list := techniqueList{technique: run.technique, matches: run.matches}
		lists = append(lists, list)
		perQuery[run.queryIndex] = append(perQuery[run.queryIndex], list)
		result.Trace = append(result.Trace, run.steps...)
	}
	if decomposeStep >= 0 {
		evidence := make([]types.QueryEvidence, len(queries))
		for i, queryText := range queries {
			evidence[i] = types.QueryEvidence{
				Query:    queryText,
				ChunkIDs: queryEvidence(perQuery[i], *req.RetrievalConfig.RRFK, *req.TopK),
			}
		}
		result.Trace[decomposeStep].Queries = &evidence
	}

	var pool []fusedChunk
	if len(lists) == 1 {
		pool = singleTechniqueChunks(lists[0].technique, lists[0].matches)
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
		reranked, step, err := s.rerank(ctx, query, pool)
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

// recordChatStep appends a first-stage chat step to result and adds its usage
// to the result's chat total, returning the step's trace index.
func recordChatStep(result *types.RetrievalResult, step types.TraceStep) int {
	result.Trace = append(result.Trace, step)
	if step.Usage != nil {
		result.ChatUsage = addUsage(result.ChatUsage, *step.Usage)
	}
	return len(result.Trace) - 1
}

// chatCall makes one non-streaming chat call for a first-stage query
// technique and returns its reply with the step skeleton that records the
// call: its step and technique, model, duration and usage. A nil reply is
// not an error; the caller decides what a missing reply means.
func (s *retrievalService) chatCall(ctx context.Context, step string, technique types.RetrievalTechnique,
	turns []types.ChatTurn,
) (*types.ChatResponse, types.TraceStep, error) {
	model := s.chat.GetModelName()
	start := time.Now()
	reply, err := s.chat.Generate(ctx, turns)
	if err != nil {
		return nil, types.TraceStep{}, err
	}
	traceStep := types.TraceStep{
		Step:       step,
		Technique:  technique,
		DurationMS: time.Since(start).Milliseconds(),
		Model:      model,
	}
	if reply != nil {
		usage := reply.Usage
		traceStep.Usage = &usage
	}
	return reply, traceStep, nil
}

// rewriteQuery turns the latest question into one standalone query with the
// chat role, using history oldest first, and returns the query to search with
// and the trace step that records the call. The reply is trimmed; a blank or
// over-long reply falls back to the raw question, so a bad rewrite can never
// send an arbitrary string to a paid search. A provider failure keeps its own
// error, which the handler maps to 502 for a rejection and 500 otherwise, the
// same as the other model calls.
func (s *retrievalService) rewriteQuery(ctx context.Context, query string,
	history []types.ConversationTurn,
) (string, types.TraceStep, error) {
	reply, step, err := s.chatCall(ctx, types.TraceStepRewriteQuery,
		types.RetrievalTechniqueQueryRewrite, rewriteTurns(history, query))
	if err != nil {
		return "", types.TraceStep{}, fmt.Errorf("rewrite query: %w", err)
	}
	rewritten := query
	if reply != nil {
		if trimmed := strings.TrimSpace(reply.Content); trimmed != "" &&
			utf8.RuneCountInString(trimmed) <= types.RetrievalQueryMaxRunes {
			rewritten = trimmed
		}
	}
	step.RewrittenQuery = rewritten
	return rewritten, step, nil
}

// decomposeQuery splits query into sub-questions with the chat role and
// returns the usable ones with the trace step that records the call. It never
// returns the original among them: the caller searches the original itself.
// A nil reply or a reply with no usable entry leaves the list empty, so only
// the original runs while the step still records the call; a provider
// failure keeps its own error, mapped like the other model calls.
func (s *retrievalService) decomposeQuery(ctx context.Context, query string) ([]string, types.TraceStep, error) {
	reply, step, err := s.chatCall(ctx, types.TraceStepDecomposeQuery,
		types.RetrievalTechniqueQueryDecomposition, decomposeTurns(query))
	if err != nil {
		return nil, types.TraceStep{}, fmt.Errorf("decompose query: %w", err)
	}
	var subQuestions []string
	if reply != nil {
		subQuestions = parseSubQuestions(reply.Content, query)
	}
	return subQuestions, step, nil
}

// techniqueRun is one (query, technique) run's matches and trace steps. The
// returned runs are in query index then request order, so the trace is
// deterministic even though the runs finish in any order.
type techniqueRun struct {
	queryIndex int
	technique  types.RetrievalTechnique
	matches    []*types.ChunkMatch
	steps      []types.TraceStep
}

// runTechniques runs every (query, technique) pair concurrently over the same
// readable set and the same candidate depth. The answer is ordered by query
// index, then the technique's request order. Any run's error fails the
// request; the caller stamps the query index on a decomposed run's steps.
func (s *retrievalService) runTechniques(ctx context.Context, techniques []types.RetrievalTechnique,
	documentIDs []uuid.UUID, queries []string, depth int,
) ([]techniqueRun, error) {
	// Resolve every runner before starting a goroutine, so an unknown
	// technique fails without leaving goroutines writing to a slice nobody
	// waits for.
	runners := make(map[types.RetrievalTechnique]techniqueRunner, len(techniques))
	for _, technique := range techniques {
		run, ok := s.runners[technique]
		if !ok {
			return nil, fmt.Errorf("no runner for retrieval technique %q", technique)
		}
		runners[technique] = run
	}
	type job struct {
		queryIndex int
		technique  types.RetrievalTechnique
		runner     techniqueRunner
	}
	jobs := make([]job, 0, len(queries)*len(techniques))
	runs := make([]techniqueRun, 0, len(queries)*len(techniques))
	for queryIndex := range queries {
		for _, technique := range techniques {
			jobs = append(jobs, job{queryIndex: queryIndex, technique: technique, runner: runners[technique]})
			runs = append(runs, techniqueRun{queryIndex: queryIndex, technique: technique})
		}
	}
	group, groupCtx := errgroup.WithContext(ctx)
	for i := range jobs {
		group.Go(func() error {
			matches, steps, err := jobs[i].runner(groupCtx, documentIDs, queries[jobs[i].queryIndex], depth)
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

// techniqueList is one ranked list of matches and the technique that produced
// it. One technique can have several lists when query decomposition searched
// with several queries.
type techniqueList struct {
	technique types.RetrievalTechnique
	matches   []*types.ChunkMatch
}

// fuseRRF combines ranked lists with Reciprocal Rank Fusion: a Chunk's score
// is the sum over every list that carries it of 1/(k+rank), with 1-based
// ranks, so a Chunk missing from a list gets nothing from it. One technique's
// score and rank are the best (lowest) rank it gave the Chunk in any list,
// with that list's score. It returns the fused list cut to topK and the
// number of distinct Chunks fused before the cut. The order is fused score
// descending, then Chunk ID ascending, so equal scores keep a deterministic
// order.
func fuseRRF(lists []techniqueList, k, topK int) ([]fusedChunk, int) {
	byChunk := make(map[uuid.UUID]*fusedChunk)
	for _, list := range lists {
		for i, match := range list.matches {
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
			rank := i + 1
			if best, ok := entry.ranks[list.technique]; !ok || rank < best {
				entry.scores[list.technique] = match.Score
				entry.ranks[list.technique] = rank
			}
			entry.score += 1 / float64(k+rank)
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

// queryEvidence returns the IDs of the Chunks one query found on its own: its
// technique lists fused with Reciprocal Rank Fusion when it ran more than one,
// otherwise its single list, cut to topK. It is taken before the global
// fusion, rerank and parent expansion, so a decomposed trace can show what
// each sub-question contributed.
func queryEvidence(lists []techniqueList, k, topK int) []uuid.UUID {
	if len(lists) == 1 {
		ids := make([]uuid.UUID, 0, min(len(lists[0].matches), topK))
		for _, match := range lists[0].matches {
			if len(ids) == topK {
				break
			}
			ids = append(ids, match.Chunk.ID)
		}
		return ids
	}
	fused, _ := fuseRRF(lists, k, topK)
	ids := make([]uuid.UUID, 0, len(fused))
	for _, entry := range fused {
		ids = append(ids, entry.chunk.ID)
	}
	return ids
}
