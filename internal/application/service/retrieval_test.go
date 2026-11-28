package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// fakeReadableDocuments answers the readable-Document query; every other
// DocumentRepository method is left to the embedded nil and would panic if
// the service called it.
type fakeReadableDocuments struct {
	interfaces.DocumentRepository
	readable     []uuid.UUID
	err          error
	gotPrincipal uuid.UUID
	calls        int
}

func (f *fakeReadableDocuments) ListReadableDocumentIDs(_ context.Context, principalID uuid.UUID) ([]uuid.UUID, error) {
	f.calls++
	f.gotPrincipal = principalID
	return f.readable, f.err
}

// fakeChunkSearch records each technique's search arguments and answers
// fixed matches. The techniques and the decomposed queries run concurrently,
// so every record is guarded and each query's matches can be pinned
// separately.
type fakeChunkSearch struct {
	interfaces.ChunkSearcher
	mu sync.Mutex

	denseMatches []*types.ChunkMatch
	bm25Matches  []*types.ChunkMatch
	// bm25MatchesByQuery answers the matches pinned to one query; a query
	// with no entry gets bm25Matches.
	bm25MatchesByQuery map[string][]*types.ChunkMatch
	denseErr           error
	bm25Err            error
	denseCalls         int
	bm25Calls          int

	denseDocumentIDs []uuid.UUID
	denseVector      []float32
	denseModel       string
	denseTopK        int

	bm25DocumentIDs []uuid.UUID
	bm25Query       string
	bm25Queries     []string
	bm25TopK        int
	bm25TopKs       []int

	parentChunks      []*types.ParentChunk
	parentErr         error
	parentCalls       int
	parentDocumentIDs []uuid.UUID
	parentIDs         []uuid.UUID
}

func (f *fakeChunkSearch) SearchDense(_ context.Context, documentIDs []uuid.UUID, queryVector []float32, model string, topK int) ([]*types.ChunkMatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denseCalls++
	f.denseDocumentIDs = append([]uuid.UUID(nil), documentIDs...)
	f.denseVector = append([]float32(nil), queryVector...)
	f.denseModel, f.denseTopK = model, topK
	return f.denseMatches, f.denseErr
}

func (f *fakeChunkSearch) SearchBM25(_ context.Context, documentIDs []uuid.UUID, query string, topK int) ([]*types.ChunkMatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bm25Calls++
	f.bm25DocumentIDs = append([]uuid.UUID(nil), documentIDs...)
	f.bm25Query, f.bm25TopK = query, topK
	f.bm25Queries = append(f.bm25Queries, query)
	f.bm25TopKs = append(f.bm25TopKs, topK)
	matches, ok := f.bm25MatchesByQuery[query]
	if !ok {
		matches = f.bm25Matches
	}
	return matches, f.bm25Err
}

func (f *fakeChunkSearch) ListParentChunks(_ context.Context, documentIDs, parentIDs []uuid.UUID) ([]*types.ParentChunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parentCalls++
	f.parentDocumentIDs = append([]uuid.UUID(nil), documentIDs...)
	f.parentIDs = append([]uuid.UUID(nil), parentIDs...)
	return f.parentChunks, f.parentErr
}

// fakeEmbedder answers one vector per text and records what it was asked to
// embed, every call in order.
type fakeEmbedder struct {
	vectors [][]float32
	err     error
	model   string
	mu      sync.Mutex
	calls   int
	got     []string
	inputs  [][]string
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.got = append([]string(nil), texts...)
	f.inputs = append(f.inputs, append([]string(nil), texts...))
	return f.vectors, f.err
}

func (f *fakeEmbedder) GetModelName() string { return f.model }

// embeddedTexts returns every text the embedder was asked for, flattened
// across calls, so a concurrent decomposed run can be compared as a set.
func (f *fakeEmbedder) embeddedTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var texts []string
	for _, input := range f.inputs {
		texts = append(texts, input...)
	}
	return texts
}

// fakeReranker answers fixed scores in the texts' order and records what it
// was asked to score. It runs once, after the candidate techniques, so its
// fields need no locking.
type fakeReranker struct {
	scores   []float64
	err      error
	model    string
	calls    int
	gotQuery string
	gotTexts []string
}

func (f *fakeReranker) Rerank(_ context.Context, query string, texts []string) ([]float64, error) {
	f.calls++
	f.gotQuery = query
	f.gotTexts = append([]string(nil), texts...)
	return f.scores, f.err
}

func (f *fakeReranker) GetModelName() string { return f.model }

// forbiddenReranker fails the test when the service calls the rerank role
// with rerank off, or with an empty pool.
type forbiddenReranker struct{ t *testing.T }

func (f *forbiddenReranker) Rerank(context.Context, string, []string) ([]float64, error) {
	f.t.Error("reranker called when there was nothing to rerank")
	return nil, nil
}

func (f *forbiddenReranker) GetModelName() string { return "forbidden" }

// callerContext is ctx with a Principal, as the router's middleware leaves it.
func callerContext(principalID uuid.UUID) context.Context {
	return types.WithPrincipal(context.Background(), types.Principal{ID: principalID, Name: "alice"})
}

// intPointer returns a pointer to n, for an explicitly given top_k.
func intPointer(n int) *int { return &n }

// expandedEntries returns the expand step's expanded list, failing when the
// step left the field out.
func expandedEntries(t *testing.T, step types.TraceStep) []types.ParentExpansion {
	t.Helper()
	if step.Expanded == nil {
		t.Fatal("expand_parents step left expanded out, want a list (empty when there are no results)")
	}
	return *step.Expanded
}

// hybridRequest is a request that runs bm25 and dense together, the shape most
// fusion tests need; a test sets TopK or the config's RRFK as it needs.
func hybridRequest(query string) *types.RetrievalRequest {
	return &types.RetrievalRequest{
		Query: query,
		RetrievalConfig: &types.RetrievalConfig{
			Techniques: []types.RetrievalTechnique{types.RetrievalTechniqueBM25, types.RetrievalTechniqueDense},
		},
	}
}

// TestEveryKnownCandidateTechniqueHasARunner guards the runner map from drifting away
// from the techniques validation admits.
func TestEveryKnownCandidateTechniqueHasARunner(t *testing.T) {
	retrieval, ok := NewRetrievalService(nil, nil, nil, nil, nil).(*retrievalService)
	if !ok {
		t.Fatal("NewRetrievalService did not return a *retrievalService")
	}
	config := &types.RetrievalConfig{Techniques: types.KnownRetrievalTechniques()}
	for _, technique := range config.CandidateTechniques() {
		if retrieval.runners[technique] == nil {
			t.Errorf("retrieval technique %q has no runner", technique)
		}
	}
}

func TestRetrieveSearchesOnlyTheCallersReadableDocuments(t *testing.T) {
	principalID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a1")
	docA := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	docB := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b2")
	chunkHighB := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")
	chunkHighA := uuid.MustParse("0190d2a8-0000-7000-8000-000000000003")
	chunkLow := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")

	docs := &fakeReadableDocuments{readable: []uuid.UUID{docA, docB}}
	// In the repository's order: score descending, ties by Chunk ID. The
	// service must not re-sort what the SQL already ordered.
	chunks := &fakeChunkSearch{denseMatches: []*types.ChunkMatch{
		{Chunk: &types.Chunk{ID: chunkHighB, DocumentID: docB, Seq: 0, Content: "near B"}, Score: 0.75},
		{Chunk: &types.Chunk{ID: chunkHighA, DocumentID: docA, Seq: 1, Content: "near A", ContextHeader: "Chương I"}, Score: 0.75},
		{Chunk: &types.Chunk{ID: chunkLow, DocumentID: docA, Seq: 2, Content: "far"}, Score: 0.25},
	}}
	embedder := &fakeEmbedder{vectors: [][]float32{{0.1, 0.2}}, model: "fake-embedding"}
	retrieval := NewRetrievalService(docs, chunks, embedder, nil, nil)

	req := &types.RetrievalRequest{Query: "  who won?  ", TopK: intPointer(5)}
	got, err := retrieval.Retrieve(callerContext(principalID), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if docs.calls != 1 || docs.gotPrincipal != principalID {
		t.Errorf("readable Documents asked %d times for %s, want once for %s", docs.calls, docs.gotPrincipal, principalID)
	}
	if !slices.Equal(embedder.got, []string{"who won?"}) {
		t.Errorf("embedded %q, want the trimmed query alone", embedder.got)
	}
	if chunks.denseCalls != 1 {
		t.Fatalf("search called %d times, want once", chunks.denseCalls)
	}
	if !slices.Equal(chunks.denseDocumentIDs, []uuid.UUID{docA, docB}) {
		t.Errorf("search document IDs = %v, want the readable set %v", chunks.denseDocumentIDs, []uuid.UUID{docA, docB})
	}
	if !slices.Equal(chunks.denseVector, []float32{0.1, 0.2}) {
		t.Errorf("search vector = %v, want the embedded query %v", chunks.denseVector, []float32{0.1, 0.2})
	}
	if chunks.denseModel != "fake-embedding" || chunks.denseTopK != 5 {
		t.Errorf("search model/top_k = %q/%d, want fake-embedding/5", chunks.denseModel, chunks.denseTopK)
	}

	// The repository's order comes through unchanged: score descending, ties
	// by Chunk ID.
	wantIDs := []uuid.UUID{chunkHighB, chunkHighA, chunkLow}
	gotIDs := make([]uuid.UUID, len(got.Results))
	for i, r := range got.Results {
		gotIDs[i] = r.ChunkID
		if r.Score != r.Scores[types.RetrievalTechniqueDense] {
			t.Errorf("result %s score %v, scores[dense] %v; want the same value", r.ChunkID, r.Score, r.Scores)
		}
		if r.Ranks[types.RetrievalTechniqueDense] != i+1 {
			t.Errorf("result %s rank = %v, want %d", r.ChunkID, r.Ranks, i+1)
		}
	}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Errorf("result order = %v, want %v", gotIDs, wantIDs)
	}
	if got.Results[0].DocumentID != docB || got.Results[0].Content != "near B" {
		t.Errorf("first result = %+v, want near B from %s", got.Results[0], docB)
	}
	if got.Results[1].ContextHeader != "Chương I" {
		t.Errorf("second result context header = %q, want Chương I", got.Results[1].ContextHeader)
	}

	if len(got.Trace) != 2 {
		t.Fatalf("trace = %+v, want the embed and the search steps", got.Trace)
	}
	if step := got.Trace[0]; step.Step != types.TraceStepEmbedQuery || step.Technique != types.RetrievalTechniqueDense ||
		step.Model != "fake-embedding" || step.DurationMS < 0 || step.Candidates != nil {
		t.Errorf("trace[0] = %+v, want the embed step naming fake-embedding and no candidate count", step)
	}
	if step := got.Trace[1]; step.Step != types.TraceStepSearch || step.Technique != types.RetrievalTechniqueDense ||
		step.Candidates == nil || *step.Candidates != 3 || step.DurationMS < 0 {
		t.Errorf("trace[1] = %+v, want the search step with 3 candidates", step)
	}
}

func TestRetrieveWithoutReadableDocumentsStillEmbedsAndSearches(t *testing.T) {
	docs := &fakeReadableDocuments{readable: []uuid.UUID{}}
	chunks := &fakeChunkSearch{}
	embedder := &fakeEmbedder{vectors: [][]float32{{1, 0}}, model: "fake-embedding"}
	retrieval := NewRetrievalService(docs, chunks, embedder, nil, nil)

	got, err := retrieval.Retrieve(callerContext(uuid.New()), &types.RetrievalRequest{Query: "who won?"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	// The same body shape as a readable set that matches nothing: both steps
	// ran, with an empty search.
	if embedder.calls != 1 || chunks.denseCalls != 1 {
		t.Errorf("embed/search called %d/%d times with nothing readable, want both once", embedder.calls, chunks.denseCalls)
	}
	if len(chunks.denseDocumentIDs) != 0 {
		t.Errorf("search document IDs = %v, want the empty readable set", chunks.denseDocumentIDs)
	}
	if got.Results == nil || len(got.Results) != 0 {
		t.Errorf("results = %+v, want empty non-nil", got.Results)
	}
	if len(got.Trace) != 2 {
		t.Fatalf("trace = %+v, want the embed and search steps even with nothing readable", got.Trace)
	}
	if step := got.Trace[1]; step.Step != types.TraceStepSearch || step.Candidates == nil || *step.Candidates != 0 {
		t.Errorf("trace[1] = %+v, want the search step with candidates 0", step)
	}
}

func TestRetrieveBM25RunsWithoutAnEmbeddingModel(t *testing.T) {
	principalID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a1")
	docA := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	docB := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b2")
	chunkHigh := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")
	chunkLow := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")

	docs := &fakeReadableDocuments{readable: []uuid.UUID{docA, docB}}
	chunks := &fakeChunkSearch{bm25Matches: []*types.ChunkMatch{
		{Chunk: &types.Chunk{ID: chunkHigh, DocumentID: docA, Seq: 1, Content: "thuế TNCN", ContextHeader: "Chương I"}, Score: 2.5},
		{Chunk: &types.Chunk{ID: chunkLow, DocumentID: docB, Seq: 0, Content: "thuế giá trị gia tăng"}, Score: 1.25},
	}}
	// nil embedder: a bm25-only request must not need models.embedding.
	retrieval := NewRetrievalService(docs, chunks, nil, nil, nil)

	req := &types.RetrievalRequest{
		Query:           "  thue tncn  ",
		TopK:            intPointer(5),
		RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{types.RetrievalTechniqueBM25}},
	}
	got, err := retrieval.Retrieve(callerContext(principalID), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if docs.calls != 1 || docs.gotPrincipal != principalID {
		t.Errorf("readable Documents asked %d times for %s, want once for %s", docs.calls, docs.gotPrincipal, principalID)
	}
	if chunks.bm25Calls != 1 || chunks.denseCalls != 0 {
		t.Errorf("bm25/dense searches called %d/%d times, want bm25 once and dense never", chunks.bm25Calls, chunks.denseCalls)
	}
	if !slices.Equal(chunks.bm25DocumentIDs, []uuid.UUID{docA, docB}) {
		t.Errorf("search document IDs = %v, want the readable set %v", chunks.bm25DocumentIDs, []uuid.UUID{docA, docB})
	}
	if chunks.bm25Query != "thue tncn" || chunks.bm25TopK != 5 {
		t.Errorf("search query/top_k = %q/%d, want the trimmed query and 5", chunks.bm25Query, chunks.bm25TopK)
	}

	// The repository's order comes through unchanged, and every result carries
	// only the bm25 score.
	wantIDs := []uuid.UUID{chunkHigh, chunkLow}
	gotIDs := make([]uuid.UUID, len(got.Results))
	for i, r := range got.Results {
		gotIDs[i] = r.ChunkID
		if r.Score != r.Scores[types.RetrievalTechniqueBM25] {
			t.Errorf("result %s score %v, scores[bm25] %v; want the same value", r.ChunkID, r.Score, r.Scores)
		}
		if _, ok := r.Scores[types.RetrievalTechniqueDense]; ok {
			t.Errorf("result %s carries a dense score, want bm25 alone: %v", r.ChunkID, r.Scores)
		}
		if r.Ranks[types.RetrievalTechniqueBM25] != i+1 {
			t.Errorf("result %s rank = %v, want %d", r.ChunkID, r.Ranks, i+1)
		}
	}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Errorf("result order = %v, want %v", gotIDs, wantIDs)
	}
	if got.Results[0].ContextHeader != "Chương I" || got.Results[0].DocumentID != docA {
		t.Errorf("first result = %+v, want the docA Chunk with its header", got.Results[0])
	}

	if len(got.Trace) != 1 {
		t.Fatalf("trace = %+v, want the search step alone", got.Trace)
	}
	if step := got.Trace[0]; step.Step != types.TraceStepSearch || step.Technique != types.RetrievalTechniqueBM25 ||
		step.Candidates == nil || *step.Candidates != 2 || step.Model != "" || step.DurationMS < 0 {
		t.Errorf("trace[0] = %+v, want the bm25 search step with 2 candidates and no model", step)
	}
}

func TestRetrieveBM25WrapsASearchFailure(t *testing.T) {
	searchErr := errors.New("postgres down")
	chunks := &fakeChunkSearch{bm25Err: searchErr}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{uuid.New()}}, chunks, nil, nil, nil)

	_, err := retrieval.Retrieve(callerContext(uuid.New()), &types.RetrievalRequest{
		Query:           "thue tncn",
		RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{types.RetrievalTechniqueBM25}},
	})
	if !errors.Is(err, searchErr) || !strings.Contains(err.Error(), "search") {
		t.Errorf("Retrieve = %v, want the BM25 search error wrapped with what it was doing", err)
	}
}

func TestRetrieveWithoutAnEmbeddingModelIsServiceUnavailable(t *testing.T) {
	docs := &fakeReadableDocuments{err: errors.New("must not be reached")}
	var embedder interfaces.Embedder // nil: no embedding role configured
	retrieval := NewRetrievalService(docs, &fakeChunkSearch{}, embedder, nil, nil)

	_, err := retrieval.Retrieve(callerContext(uuid.New()), &types.RetrievalRequest{Query: "who won?"})
	appErr, ok := apperrors.IsAppError(err)
	if !ok {
		t.Fatalf("Retrieve = %v, want an AppError", err)
	}
	if appErr.Code != apperrors.ErrServiceUnavailable || appErr.HTTPCode != 503 {
		t.Errorf("error code/status = %d/%d, want %d/503", appErr.Code, appErr.HTTPCode, apperrors.ErrServiceUnavailable)
	}
	if !strings.Contains(appErr.Message, "models.embedding") {
		t.Errorf("message %q does not name models.embedding", appErr.Message)
	}
	if docs.calls != 0 {
		t.Errorf("readable Documents asked %d times without an embedding model, want none", docs.calls)
	}
}

func TestRetrieveWrapsAnEmbeddingFailure(t *testing.T) {
	embedErr := errors.New("provider down")
	embedder := &fakeEmbedder{err: embedErr}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{uuid.New()}}, &fakeChunkSearch{}, embedder, nil, nil)

	_, err := retrieval.Retrieve(callerContext(uuid.New()), &types.RetrievalRequest{Query: "who won?"})
	if !errors.Is(err, embedErr) || !strings.Contains(err.Error(), "embed query") {
		t.Errorf("Retrieve = %v, want the embedder's error wrapped with what it was doing", err)
	}
	if _, ok := apperrors.IsAppError(err); ok {
		t.Errorf("Retrieve = %v, want a plain error so the handler answers 500", err)
	}
}

func TestRetrieveWrapsASearchFailure(t *testing.T) {
	searchErr := errors.New("postgres down")
	chunks := &fakeChunkSearch{denseErr: searchErr}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{uuid.New()}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, nil, nil)

	_, err := retrieval.Retrieve(callerContext(uuid.New()), &types.RetrievalRequest{Query: "who won?"})
	if !errors.Is(err, searchErr) || !strings.Contains(err.Error(), "search") {
		t.Errorf("Retrieve = %v, want the search error wrapped with what it was doing", err)
	}
}

func TestRetrieveRejectsAnInvalidRequestBeforeTouchingItsDependencies(t *testing.T) {
	for name, req := range map[string]*types.RetrievalRequest{
		"blank query":       {Query: "   "},
		"top_k zero":        {Query: "who won?", TopK: intPointer(0)},
		"top_k too large":   {Query: "who won?", TopK: intPointer(101)},
		"no techniques":     {Query: "who won?", RetrievalConfig: &types.RetrievalConfig{}},
		"unknown technique": {Query: "who won?", RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{"foo"}}},
		"rerank alone":      {Query: "who won?", RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{types.RetrievalTechniqueRerank}}},
		"rerank_candidates too small": {
			Query: "who won?",
			TopK:  intPointer(10),
			RetrievalConfig: &types.RetrievalConfig{
				Techniques:       []types.RetrievalTechnique{types.RetrievalTechniqueBM25, types.RetrievalTechniqueRerank},
				RerankCandidates: intPointer(5),
			},
		},
		"duplicate":  {Query: "who won?", RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{types.RetrievalTechniqueDense, types.RetrievalTechniqueDense}}},
		"rrf_k zero": {Query: "who won?", RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{types.RetrievalTechniqueBM25}, RRFK: intPointer(0)}},
	} {
		t.Run(name, func(t *testing.T) {
			docs := &fakeReadableDocuments{err: errors.New("must not be reached")}
			chunks := &fakeChunkSearch{}
			embedder := &fakeEmbedder{}
			retrieval := NewRetrievalService(docs, chunks, embedder, nil, nil)

			_, err := retrieval.Retrieve(callerContext(uuid.New()), req)
			appErr, ok := apperrors.IsAppError(err)
			if !ok {
				t.Fatalf("Retrieve = %v, want a validation AppError", err)
			}
			if appErr.Code != apperrors.ErrValidation || appErr.HTTPCode != 400 {
				t.Errorf("error code/status = %d/%d, want %d/400", appErr.Code, appErr.HTTPCode, apperrors.ErrValidation)
			}
			if docs.calls+embedder.calls+chunks.denseCalls+chunks.bm25Calls != 0 {
				t.Errorf("dependencies called %d/%d/%d/%d times for an invalid request, want none",
					docs.calls, embedder.calls, chunks.denseCalls, chunks.bm25Calls)
			}
		})
	}
}

func TestFuseRRFCombinesRanks(t *testing.T) {
	chunkA := &types.Chunk{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")}
	chunkB := &types.Chunk{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")}
	chunkC := &types.Chunk{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000003")}
	lists := []techniqueList{
		{technique: types.RetrievalTechniqueBM25, matches: []*types.ChunkMatch{
			{Chunk: chunkA, Score: 5},
			{Chunk: chunkB, Score: 3},
		}},
		{technique: types.RetrievalTechniqueDense, matches: []*types.ChunkMatch{
			{Chunk: chunkB, Score: 0.9},
			{Chunk: chunkC, Score: 0.8},
		}},
	}
	for _, tc := range []struct {
		name string
		k    int
		want map[uuid.UUID]float64
	}{
		{
			name: "k 60",
			k:    60,
			want: map[uuid.UUID]float64{
				chunkA.ID: 1.0 / 61,
				chunkB.ID: 1.0/62 + 1.0/61,
				chunkC.ID: 1.0 / 62,
			},
		},
		{
			name: "k 1",
			k:    1,
			want: map[uuid.UUID]float64{
				chunkA.ID: 1.0 / 2,
				chunkB.ID: 1.0/3 + 1.0/2,
				chunkC.ID: 1.0 / 3,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, distinct := fuseRRF(lists, tc.k, 10)
			if distinct != 3 {
				t.Errorf("distinct = %d, want 3", distinct)
			}
			if len(got) != 3 {
				t.Fatalf("fused %d entries, want 3", len(got))
			}
			// B appears in both lists and leads; A and C each appear once.
			wantOrder := []uuid.UUID{chunkB.ID, chunkA.ID, chunkC.ID}
			for i, entry := range got {
				if entry.chunk.ID != wantOrder[i] {
					t.Errorf("entry %d is %s, want %s", i, entry.chunk.ID, wantOrder[i])
				}
				if diff := math.Abs(entry.score - tc.want[entry.chunk.ID]); diff > 1e-12 {
					t.Errorf("%s score = %v, want %v", entry.chunk.ID, entry.score, tc.want[entry.chunk.ID])
				}
			}
			// Each entry carries its own technique scores and 1-based ranks.
			if got[0].scores[types.RetrievalTechniqueBM25] != 3 || got[0].scores[types.RetrievalTechniqueDense] != 0.9 {
				t.Errorf("B scores = %v, want bm25 3 and dense 0.9", got[0].scores)
			}
			if got[0].ranks[types.RetrievalTechniqueBM25] != 2 || got[0].ranks[types.RetrievalTechniqueDense] != 1 {
				t.Errorf("B ranks = %v, want bm25 2 and dense 1", got[0].ranks)
			}
			if _, ok := got[1].scores[types.RetrievalTechniqueDense]; ok {
				t.Errorf("A scores = %v, want bm25 alone", got[1].scores)
			}
			if got[1].ranks[types.RetrievalTechniqueBM25] != 1 || len(got[1].ranks) != 1 {
				t.Errorf("A ranks = %v, want bm25 1 alone", got[1].ranks)
			}
		})
	}
}

func TestFuseRRFOrdersTiesByChunkID(t *testing.T) {
	// The list iterated first is bm25, so B is inserted first; equal scores
	// must still order by Chunk ID.
	chunkA := &types.Chunk{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")}
	chunkB := &types.Chunk{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")}
	got, _ := fuseRRF([]techniqueList{
		{technique: types.RetrievalTechniqueBM25, matches: []*types.ChunkMatch{{Chunk: chunkB, Score: 4}}},
		{technique: types.RetrievalTechniqueDense, matches: []*types.ChunkMatch{{Chunk: chunkA, Score: 0.9}}},
	}, 60, 10)
	if len(got) != 2 || got[0].chunk.ID != chunkA.ID || got[1].chunk.ID != chunkB.ID {
		t.Fatalf("fused order = %+v, want the smaller Chunk ID first on a score tie", got)
	}
}

func TestFuseRRFCutsToTopK(t *testing.T) {
	chunks := []*types.Chunk{
		{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")},
		{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")},
		{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000003")},
	}
	got, distinct := fuseRRF([]techniqueList{
		{technique: types.RetrievalTechniqueBM25, matches: []*types.ChunkMatch{
			{Chunk: chunks[0], Score: 4},
			{Chunk: chunks[1], Score: 3},
			{Chunk: chunks[2], Score: 2},
		}},
	}, 60, 2)
	if distinct != 3 {
		t.Errorf("distinct = %d, want the 3 Chunks fused before the cut", distinct)
	}
	if len(got) != 2 {
		t.Fatalf("fused %d entries, want the 2 top_k asks for", len(got))
	}
	if got[0].chunk.ID != chunks[0].ID || got[1].chunk.ID != chunks[1].ID {
		t.Errorf("fused order = %s, %s; want the two highest-ranked", got[0].chunk.ID, got[1].chunk.ID)
	}
}

func TestFuseRRFSumsOneTechniqueAcrossListsAndKeepsItsBestRank(t *testing.T) {
	chunkA := &types.Chunk{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")}
	chunkB := &types.Chunk{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")}
	// One technique with two lists, as a decomposed run produces. A leads
	// the first list and B the second, so both sum rank 1 and rank 2: equal
	// fused scores, ordered by Chunk ID.
	got, distinct := fuseRRF([]techniqueList{
		{technique: types.RetrievalTechniqueBM25, matches: []*types.ChunkMatch{
			{Chunk: chunkA, Score: 5},
			{Chunk: chunkB, Score: 3},
		}},
		{technique: types.RetrievalTechniqueBM25, matches: []*types.ChunkMatch{
			{Chunk: chunkB, Score: 8},
			{Chunk: chunkA, Score: 2},
		}},
	}, 60, 10)
	if distinct != 2 || len(got) != 2 {
		t.Fatalf("fused %d entries (%d distinct), want both Chunks", len(got), distinct)
	}
	if got[0].chunk.ID != chunkA.ID || got[1].chunk.ID != chunkB.ID {
		t.Errorf("fused order = %s, %s; want the smaller Chunk ID first on an equal score",
			got[0].chunk.ID, got[1].chunk.ID)
	}
	wantScore := 1.0/61 + 1.0/62
	for _, entry := range got {
		if diff := math.Abs(entry.score - wantScore); diff > 1e-12 {
			t.Errorf("%s score = %v, want both lists' contributions summed to %v", entry.chunk.ID, entry.score, wantScore)
		}
		if entry.ranks[types.RetrievalTechniqueBM25] != 1 {
			t.Errorf("%s bm25 rank = %d, want the best rank 1 across the lists",
				entry.chunk.ID, entry.ranks[types.RetrievalTechniqueBM25])
		}
	}
	// Each keeps the score of the list that gave it its best rank.
	if got[0].scores[types.RetrievalTechniqueBM25] != 5 {
		t.Errorf("A bm25 score = %v, want 5 from its best-ranked list", got[0].scores[types.RetrievalTechniqueBM25])
	}
	if got[1].scores[types.RetrievalTechniqueBM25] != 8 {
		t.Errorf("B bm25 score = %v, want 8 from its best-ranked list", got[1].scores[types.RetrievalTechniqueBM25])
	}
}

func TestRetrieveHybridRunsBothOverTheReadableSetAndFuses(t *testing.T) {
	principalID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a1")
	docA := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	docB := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b2")
	chunkA := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001") // bm25 first, dense second
	chunkB := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002") // dense alone
	chunkC := uuid.MustParse("0190d2a8-0000-7000-8000-000000000003") // bm25 alone, cut by top_k

	docs := &fakeReadableDocuments{readable: []uuid.UUID{docA, docB}}
	chunks := &fakeChunkSearch{
		denseMatches: []*types.ChunkMatch{
			{Chunk: &types.Chunk{ID: chunkB, DocumentID: docA, Seq: 0, Content: "near B"}, Score: 0.9},
			{Chunk: &types.Chunk{ID: chunkA, DocumentID: docA, Seq: 1, Content: "near A", ContextHeader: "Chương I"}, Score: 0.8},
		},
		bm25Matches: []*types.ChunkMatch{
			{Chunk: &types.Chunk{ID: chunkA, DocumentID: docA, Seq: 1, Content: "near A", ContextHeader: "Chương I"}, Score: 4},
			{Chunk: &types.Chunk{ID: chunkC, DocumentID: docB, Seq: 0, Content: "far"}, Score: 2},
		},
	}
	embedder := &fakeEmbedder{vectors: [][]float32{{0.1, 0.2}}, model: "fake-embedding"}
	retrieval := NewRetrievalService(docs, chunks, embedder, nil, nil)

	req := hybridRequest("  who won?  ")
	req.TopK = intPointer(2)
	got, err := retrieval.Retrieve(callerContext(principalID), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if docs.calls != 1 || docs.gotPrincipal != principalID {
		t.Errorf("readable Documents asked %d times for %s, want once for %s", docs.calls, docs.gotPrincipal, principalID)
	}
	// Both lists are fetched over the same readable set, and deeper than
	// top_k so fusion has candidates to re-order.
	if chunks.bm25Calls != 1 || chunks.denseCalls != 1 {
		t.Fatalf("bm25/dense searches called %d/%d times, want both once", chunks.bm25Calls, chunks.denseCalls)
	}
	if !slices.Equal(chunks.bm25DocumentIDs, []uuid.UUID{docA, docB}) ||
		!slices.Equal(chunks.denseDocumentIDs, []uuid.UUID{docA, docB}) {
		t.Errorf("search document IDs = %v/%v, want the readable set %v",
			chunks.bm25DocumentIDs, chunks.denseDocumentIDs, []uuid.UUID{docA, docB})
	}
	if chunks.bm25TopK != types.RetrievalFusionCandidates || chunks.denseTopK != types.RetrievalFusionCandidates {
		t.Errorf("search top_k = %d/%d, want the fusion depth %d",
			chunks.bm25TopK, chunks.denseTopK, types.RetrievalFusionCandidates)
	}
	if !slices.Equal(embedder.got, []string{"who won?"}) {
		t.Errorf("embedded %q, want the trimmed query alone", embedder.got)
	}

	if len(got.Results) != 2 {
		t.Fatalf("results = %+v, want the fused top 2", got.Results)
	}
	if got.Results[0].ChunkID != chunkA || got.Results[1].ChunkID != chunkB {
		t.Fatalf("result order = %s, %s; want A then B", got.Results[0].ChunkID, got.Results[1].ChunkID)
	}
	first := got.Results[0]
	if diff := math.Abs(first.Score - (1.0/62 + 1.0/61)); diff > 1e-12 {
		t.Errorf("A score = %v, want the RRF sum 1/62 + 1/61", first.Score)
	}
	if first.Scores[types.RetrievalTechniqueBM25] != 4 || first.Scores[types.RetrievalTechniqueDense] != 0.8 {
		t.Errorf("A scores = %v, want bm25 4 and dense 0.8", first.Scores)
	}
	if first.Ranks[types.RetrievalTechniqueBM25] != 1 || first.Ranks[types.RetrievalTechniqueDense] != 2 {
		t.Errorf("A ranks = %v, want bm25 1 and dense 2", first.Ranks)
	}
	second := got.Results[1]
	if diff := math.Abs(second.Score - 1.0/61); diff > 1e-12 {
		t.Errorf("B score = %v, want the RRF sum 1/61", second.Score)
	}
	if _, ok := second.Scores[types.RetrievalTechniqueBM25]; ok {
		t.Errorf("B scores = %v, want dense alone", second.Scores)
	}
	if len(second.Ranks) != 1 || second.Ranks[types.RetrievalTechniqueDense] != 1 {
		t.Errorf("B ranks = %v, want dense 1 alone", second.Ranks)
	}

	// Each technique keeps its own steps, then one fuse step reports how many
	// distinct Chunks went in — C included, though top_k cut it.
	if len(got.Trace) != 4 {
		t.Fatalf("trace = %+v, want bm25's search, dense's two steps and the fuse step", got.Trace)
	}
	if step := got.Trace[0]; step.Step != types.TraceStepSearch || step.Technique != types.RetrievalTechniqueBM25 ||
		step.Candidates == nil || *step.Candidates != 2 {
		t.Errorf("trace[0] = %+v, want the bm25 search step with 2 candidates", step)
	}
	if step := got.Trace[1]; step.Step != types.TraceStepEmbedQuery || step.Technique != types.RetrievalTechniqueDense ||
		step.Model != "fake-embedding" {
		t.Errorf("trace[1] = %+v, want the dense embed step", step)
	}
	if step := got.Trace[2]; step.Step != types.TraceStepSearch || step.Technique != types.RetrievalTechniqueDense ||
		step.Candidates == nil || *step.Candidates != 2 {
		t.Errorf("trace[2] = %+v, want the dense search step with 2 candidates", step)
	}
	if step := got.Trace[3]; step.Step != types.TraceStepFuse || step.Technique != types.RetrievalTechniqueRRF ||
		step.Candidates == nil || *step.Candidates != 3 || step.DurationMS < 0 {
		t.Errorf("trace[3] = %+v, want the fuse step reporting 3 distinct Chunks", step)
	}
}

func TestRetrieveHybridFetchesTopKWhenItExceedsTheFusionDepth(t *testing.T) {
	chunks := &fakeChunkSearch{}
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, nil, nil)

	req := hybridRequest("who won?")
	req.TopK = intPointer(80)
	if _, err := retrieval.Retrieve(callerContext(uuid.New()), req); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	// max(top_k, RetrievalFusionCandidates) leaves top_k alone when it is
	// already deeper than the fusion pool.
	if chunks.bm25TopK != 80 || chunks.denseTopK != 80 {
		t.Errorf("search top_k = %d/%d, want 80 for both", chunks.bm25TopK, chunks.denseTopK)
	}
}

func TestRetrieveHybridWithoutAnEmbeddingModelIsServiceUnavailable(t *testing.T) {
	docs := &fakeReadableDocuments{err: errors.New("must not be reached")}
	retrieval := NewRetrievalService(docs, &fakeChunkSearch{}, nil, nil, nil)

	_, err := retrieval.Retrieve(callerContext(uuid.New()), hybridRequest("who won?"))
	appErr, ok := apperrors.IsAppError(err)
	if !ok {
		t.Fatalf("Retrieve = %v, want an AppError", err)
	}
	if appErr.Code != apperrors.ErrServiceUnavailable || appErr.HTTPCode != 503 {
		t.Errorf("error code/status = %d/%d, want %d/503", appErr.Code, appErr.HTTPCode, apperrors.ErrServiceUnavailable)
	}
	if docs.calls != 0 {
		t.Errorf("readable Documents asked %d times without an embedding model, want none", docs.calls)
	}
}

func TestRetrieveHybridFailsWhenOneTechniqueFails(t *testing.T) {
	searchErr := errors.New("postgres down")
	chunks := &fakeChunkSearch{bm25Err: searchErr}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{uuid.New()}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, nil, nil)

	_, err := retrieval.Retrieve(callerContext(uuid.New()), hybridRequest("who won?"))
	if !errors.Is(err, searchErr) || !strings.Contains(err.Error(), "bm25") {
		t.Errorf("Retrieve = %v, want the BM25 search error wrapped with what it was doing", err)
	}
}

// rerankRequest is a request that runs the named candidate techniques and
// then rerank; top_k and rerank_candidates are set by the test.
func rerankRequest(query string, techniques ...types.RetrievalTechnique) *types.RetrievalRequest {
	return &types.RetrievalRequest{
		Query: query,
		RetrievalConfig: &types.RetrievalConfig{
			Techniques: append(append([]types.RetrievalTechnique(nil), techniques...), types.RetrievalTechniqueRerank),
		},
	}
}

// parentChildRequest is a request that runs the named candidate techniques and
// then parent_child; top_k is set by the test.
func parentChildRequest(query string, techniques ...types.RetrievalTechnique) *types.RetrievalRequest {
	return &types.RetrievalRequest{
		Query: query,
		RetrievalConfig: &types.RetrievalConfig{
			Techniques: append(append([]types.RetrievalTechnique(nil), techniques...), types.RetrievalTechniqueParentChild),
		},
	}
}

func TestRetrieveSingleTechniqueReranksByTheRerankScores(t *testing.T) {
	principalID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a1")
	docA := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	docB := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b2")
	chunkA := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	chunkB := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")

	docs := &fakeReadableDocuments{readable: []uuid.UUID{docA, docB}}
	chunks := &fakeChunkSearch{denseMatches: []*types.ChunkMatch{
		{Chunk: &types.Chunk{ID: chunkA, DocumentID: docA, Seq: 0, Content: "near A", ContextHeader: "Chương I"}, Score: 0.9},
		{Chunk: &types.Chunk{ID: chunkB, DocumentID: docB, Seq: 0, Content: "near B"}, Score: 0.8},
	}}
	reranker := &fakeReranker{scores: []float64{0.1, 0.95}, model: "fake-rerank"}
	retrieval := NewRetrievalService(docs, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, reranker, nil)

	req := rerankRequest("  who won?  ", types.RetrievalTechniqueDense)
	req.TopK = intPointer(5)
	got, err := retrieval.Retrieve(callerContext(principalID), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	// The search runs deep enough to fill the default rerank pool.
	if chunks.denseTopK != types.RetrievalRerankCandidatesDefault {
		t.Errorf("dense top_k = %d, want the rerank pool %d", chunks.denseTopK, types.RetrievalRerankCandidatesDefault)
	}
	if reranker.calls != 1 || reranker.gotQuery != "who won?" {
		t.Errorf("reranker called %d times with query %q, want once with the trimmed query",
			reranker.calls, reranker.gotQuery)
	}
	// The reranker scores the same text the Chunk was embedded from.
	if want := []string{"Chương I\n\nnear A", "near B"}; !slices.Equal(reranker.gotTexts, want) {
		t.Errorf("reranked texts = %q, want %q", reranker.gotTexts, want)
	}

	if len(got.Results) != 2 || got.Results[0].ChunkID != chunkB || got.Results[1].ChunkID != chunkA {
		t.Fatalf("result order = %+v, want B then A by rerank score", got.Results)
	}
	first := got.Results[0]
	if first.Score != 0.95 || first.Scores[types.RetrievalTechniqueRerank] != 0.95 {
		t.Errorf("first score = %v, scores = %v; want the rerank score 0.95", first.Score, first.Scores)
	}
	if first.Scores[types.RetrievalTechniqueDense] != 0.8 {
		t.Errorf("first scores = %v, want the first-stage dense score 0.8 kept", first.Scores)
	}
	if first.Ranks[types.RetrievalTechniqueRerank] != 1 || first.Ranks[types.RetrievalTechniqueDense] != 2 {
		t.Errorf("first ranks = %v, want rerank 1 and dense 2", first.Ranks)
	}
	second := got.Results[1]
	if second.Score != 0.1 || second.Scores[types.RetrievalTechniqueDense] != 0.9 {
		t.Errorf("second score/scores = %v/%v, want rerank 0.1 over dense 0.9", second.Score, second.Scores)
	}
	if second.Ranks[types.RetrievalTechniqueRerank] != 2 || second.Ranks[types.RetrievalTechniqueDense] != 1 {
		t.Errorf("second ranks = %v, want rerank 2 and dense 1", second.Ranks)
	}

	// The rerank step comes after the search steps and reports what it scored.
	if len(got.Trace) != 3 {
		t.Fatalf("trace = %+v, want the embed, search and rerank steps", got.Trace)
	}
	step := got.Trace[2]
	if step.Step != types.TraceStepRerank || step.Technique != types.RetrievalTechniqueRerank ||
		step.Model != "fake-rerank" || step.Candidates == nil || *step.Candidates != 2 || step.DurationMS < 0 {
		t.Errorf("trace[2] = %+v, want the rerank step naming fake-rerank and 2 candidates", step)
	}
}

func TestRetrieveHybridReranksTheFusedPool(t *testing.T) {
	docA := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	docB := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b2")
	chunkA := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001") // fused first
	chunkB := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002") // fused second
	chunkC := uuid.MustParse("0190d2a8-0000-7000-8000-000000000003") // fused third, cut by the pool

	chunks := &fakeChunkSearch{
		bm25Matches: []*types.ChunkMatch{
			{Chunk: &types.Chunk{ID: chunkA, DocumentID: docA, Seq: 0, Content: "near A"}, Score: 4},
			{Chunk: &types.Chunk{ID: chunkC, DocumentID: docB, Seq: 0, Content: "far C"}, Score: 2},
		},
		denseMatches: []*types.ChunkMatch{
			{Chunk: &types.Chunk{ID: chunkB, DocumentID: docB, Seq: 0, Content: "near B"}, Score: 0.9},
			{Chunk: &types.Chunk{ID: chunkA, DocumentID: docA, Seq: 0, Content: "near A"}, Score: 0.8},
		},
	}
	reranker := &fakeReranker{scores: []float64{0.2, 0.7}, model: "fake-rerank"}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docA, docB}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, reranker, nil)

	req := rerankRequest("who won?", types.RetrievalTechniqueBM25, types.RetrievalTechniqueDense)
	req.TopK = intPointer(1)
	req.RetrievalConfig.RerankCandidates = intPointer(2)
	got, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	// Fusion still fetches its usual depth; the pool cut is what rerank
	// changes.
	if chunks.bm25TopK != types.RetrievalFusionCandidates || chunks.denseTopK != types.RetrievalFusionCandidates {
		t.Errorf("search top_k = %d/%d, want the fusion depth %d",
			chunks.bm25TopK, chunks.denseTopK, types.RetrievalFusionCandidates)
	}
	// The pool is the fused list cut to rerank_candidates, in fused order.
	if want := []string{"near A", "near B"}; !slices.Equal(reranker.gotTexts, want) {
		t.Errorf("reranked texts = %q, want the fused pool %q", reranker.gotTexts, want)
	}
	if len(got.Results) != 1 || got.Results[0].ChunkID != chunkB {
		t.Fatalf("results = %+v, want B first after rerank and cut to top_k 1", got.Results)
	}
	first := got.Results[0]
	if first.Score != 0.7 || first.Scores[types.RetrievalTechniqueDense] != 0.9 ||
		first.Ranks[types.RetrievalTechniqueRerank] != 1 || first.Ranks[types.RetrievalTechniqueDense] != 1 {
		t.Errorf("first = %+v, want rerank 0.7 first with its dense score and ranks kept", first)
	}

	if len(got.Trace) != 5 {
		t.Fatalf("trace = %+v, want bm25's search, dense's two steps, the fuse and the rerank steps", got.Trace)
	}
	if step := got.Trace[3]; step.Step != types.TraceStepFuse || step.Candidates == nil || *step.Candidates != 3 {
		t.Errorf("trace[3] = %+v, want the fuse step over 3 distinct Chunks", step)
	}
	if step := got.Trace[4]; step.Step != types.TraceStepRerank || step.Technique != types.RetrievalTechniqueRerank ||
		step.Model != "fake-rerank" || step.Candidates == nil || *step.Candidates != 2 {
		t.Errorf("trace[4] = %+v, want the rerank step after the fuse step with 2 candidates", step)
	}
}

func TestRetrieveRerankCandidatesCapsThePoolAndDeepensTheSearch(t *testing.T) {
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	matches := make([]*types.ChunkMatch, 5)
	for i := range matches {
		matches[i] = &types.ChunkMatch{
			Chunk: &types.Chunk{ID: uuid.MustParse(fmt.Sprintf("0190d2a8-0000-7000-8000-%012d", i+1)),
				DocumentID: docID, Seq: i, Content: fmt.Sprintf("chunk %d", i+1)},
			Score: float64(5 - i),
		}
	}
	chunks := &fakeChunkSearch{denseMatches: matches}
	reranker := &fakeReranker{scores: []float64{0.1, 0.2, 0.9}, model: "fake-rerank"}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docID}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, reranker, nil)

	req := rerankRequest("who won?", types.RetrievalTechniqueDense)
	req.TopK = intPointer(1)
	req.RetrievalConfig.RerankCandidates = intPointer(3)
	got, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	// The candidate techniques fetch at least the pool size, and only the
	// pool reaches the reranker.
	if chunks.denseTopK != 3 {
		t.Errorf("dense top_k = %d, want the pool size 3", chunks.denseTopK)
	}
	if len(reranker.gotTexts) != 3 {
		t.Fatalf("reranked %d texts, want the pool capped at 3: %q", len(reranker.gotTexts), reranker.gotTexts)
	}
	if want := []string{"chunk 1", "chunk 2", "chunk 3"}; !slices.Equal(reranker.gotTexts, want) {
		t.Errorf("reranked texts = %q, want the first 3 in first-stage order %q", reranker.gotTexts, want)
	}
	if len(got.Results) != 1 || got.Results[0].ChunkID != matches[2].Chunk.ID {
		t.Errorf("results = %+v, want the third pool Chunk its rerank score ranks first", got.Results)
	}
}

func TestRetrieveRerankOrdersScoreTiesByChunkID(t *testing.T) {
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunkA := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	chunkB := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")
	chunks := &fakeChunkSearch{denseMatches: []*types.ChunkMatch{
		// B comes first from the search; an equal rerank score must order by
		// Chunk ID, so A leads.
		{Chunk: &types.Chunk{ID: chunkB, DocumentID: docID, Content: "near B"}, Score: 0.9},
		{Chunk: &types.Chunk{ID: chunkA, DocumentID: docID, Content: "near A"}, Score: 0.8},
	}}
	reranker := &fakeReranker{scores: []float64{0.5, 0.5}, model: "fake-rerank"}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docID}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, reranker, nil)

	got, err := retrieval.Retrieve(callerContext(uuid.New()), rerankRequest("who won?", types.RetrievalTechniqueDense))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got.Results) != 2 || got.Results[0].ChunkID != chunkA || got.Results[1].ChunkID != chunkB {
		t.Fatalf("result order = %+v, want the smaller Chunk ID first on a rerank score tie", got.Results)
	}
	if got.Results[0].Ranks[types.RetrievalTechniqueRerank] != 1 ||
		got.Results[1].Ranks[types.RetrievalTechniqueRerank] != 2 {
		t.Errorf("rerank ranks = %v/%v, want 1/2 in the tied order",
			got.Results[0].Ranks, got.Results[1].Ranks)
	}
}

func TestRetrieveWithRerankOffNeverCallsTheRerankModel(t *testing.T) {
	docA := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunkA := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	chunkB := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")
	chunks := &fakeChunkSearch{denseMatches: []*types.ChunkMatch{
		{Chunk: &types.Chunk{ID: chunkA, DocumentID: docA, Content: "near A"}, Score: 0.9},
		{Chunk: &types.Chunk{ID: chunkB, DocumentID: docA, Content: "near B"}, Score: 0.8},
	}}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docA}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, &forbiddenReranker{t: t}, nil)

	req := &types.RetrievalRequest{Query: "who won?", TopK: intPointer(5)}
	got, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	// Nothing about a rerank-off request changes: depth, order and trace are
	// the ones it had before rerank existed.
	if chunks.denseTopK != 5 {
		t.Errorf("dense top_k = %d, want top_k 5 without rerank", chunks.denseTopK)
	}
	if len(got.Results) != 2 || got.Results[0].ChunkID != chunkA || got.Results[1].ChunkID != chunkB {
		t.Errorf("results = %+v, want the dense order A then B", got.Results)
	}
	if got.Results[0].Score != 0.9 || got.Results[0].Ranks[types.RetrievalTechniqueDense] != 1 {
		t.Errorf("first result = %+v, want its dense score and rank", got.Results[0])
	}
	if len(got.Trace) != 2 {
		t.Errorf("trace = %+v, want the embed and search steps alone", got.Trace)
	}
}

func TestRetrieveHybridWithRerankOffIgnoresRerankCandidates(t *testing.T) {
	docA := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	docB := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b2")
	chunkA := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	chunkB := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")
	chunks := &fakeChunkSearch{
		bm25Matches: []*types.ChunkMatch{
			{Chunk: &types.Chunk{ID: chunkA, DocumentID: docA, Content: "near A"}, Score: 4},
		},
		denseMatches: []*types.ChunkMatch{
			{Chunk: &types.Chunk{ID: chunkB, DocumentID: docB, Content: "near B"}, Score: 0.9},
		},
	}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docA, docB}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, &forbiddenReranker{t: t}, nil)

	without := hybridRequest("who won?")
	without.TopK = intPointer(2)
	gotWithout, err := retrieval.Retrieve(callerContext(uuid.New()), without)
	if err != nil {
		t.Fatalf("Retrieve without rerank_candidates: %v", err)
	}
	if chunks.bm25TopK != types.RetrievalFusionCandidates || chunks.denseTopK != types.RetrievalFusionCandidates {
		t.Errorf("search top_k = %d/%d, want the fusion depth %d without rerank",
			chunks.bm25TopK, chunks.denseTopK, types.RetrievalFusionCandidates)
	}

	with := hybridRequest("who won?")
	with.TopK = intPointer(2)
	with.RetrievalConfig.RerankCandidates = intPointer(3)
	gotWith, err := retrieval.Retrieve(callerContext(uuid.New()), with)
	if err != nil {
		t.Fatalf("Retrieve with rerank_candidates: %v", err)
	}
	if chunks.bm25TopK != types.RetrievalFusionCandidates || chunks.denseTopK != types.RetrievalFusionCandidates {
		t.Errorf("search top_k = %d/%d, want the fusion depth %d with rerank off",
			chunks.bm25TopK, chunks.denseTopK, types.RetrievalFusionCandidates)
	}

	if !reflect.DeepEqual(gotWith.Results, gotWithout.Results) {
		t.Errorf("results with rerank_candidates = %+v, want the same as without %+v",
			gotWith.Results, gotWithout.Results)
	}
	if len(gotWith.Trace) != len(gotWithout.Trace) {
		t.Fatalf("trace with rerank_candidates = %+v, want the same steps as without %+v",
			gotWith.Trace, gotWithout.Trace)
	}
	for i := range gotWith.Trace {
		with, without := gotWith.Trace[i], gotWithout.Trace[i]
		// Durations are wall-clock and may differ between runs.
		with.DurationMS, without.DurationMS = 0, 0
		if !reflect.DeepEqual(with, without) {
			t.Errorf("trace[%d] with rerank_candidates = %+v, want the same as without %+v", i, with, without)
		}
	}
}

func TestRetrieveWithRerankWithoutARerankModelIsServiceUnavailable(t *testing.T) {
	docs := &fakeReadableDocuments{err: errors.New("must not be reached")}
	retrieval := NewRetrievalService(docs, &fakeChunkSearch{}, &fakeEmbedder{model: "fake-embedding"}, nil, nil)

	req := rerankRequest("who won?", types.RetrievalTechniqueDense)
	_, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	appErr, ok := apperrors.IsAppError(err)
	if !ok {
		t.Fatalf("Retrieve = %v, want an AppError", err)
	}
	if appErr.Code != apperrors.ErrServiceUnavailable || appErr.HTTPCode != 503 {
		t.Errorf("error code/status = %d/%d, want %d/503", appErr.Code, appErr.HTTPCode, apperrors.ErrServiceUnavailable)
	}
	if !strings.Contains(appErr.Message, "models.rerank") {
		t.Errorf("message %q does not name models.rerank", appErr.Message)
	}
	if docs.calls != 0 {
		t.Errorf("readable Documents asked %d times without a rerank model, want none", docs.calls)
	}
}

func TestRetrieveWrapsARerankFailure(t *testing.T) {
	rerankErr := errors.New("provider down")
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunks := &fakeChunkSearch{denseMatches: []*types.ChunkMatch{{
		Chunk: &types.Chunk{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000001"), DocumentID: docID, Content: "near"},
		Score: 0.9,
	}}}
	reranker := &fakeReranker{err: rerankErr}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docID}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, reranker, nil)

	_, err := retrieval.Retrieve(callerContext(uuid.New()), rerankRequest("who won?", types.RetrievalTechniqueDense))
	if !errors.Is(err, rerankErr) || !strings.Contains(err.Error(), "rerank") {
		t.Errorf("Retrieve = %v, want the reranker's error wrapped with what it was doing", err)
	}
	if _, ok := apperrors.IsAppError(err); ok {
		t.Errorf("Retrieve = %v, want a plain error so the handler answers 500", err)
	}
}

func TestRetrieveRejectsARerankScoreCountMismatch(t *testing.T) {
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunks := &fakeChunkSearch{denseMatches: []*types.ChunkMatch{
		{Chunk: &types.Chunk{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000001"), DocumentID: docID, Content: "one"}, Score: 0.9},
		{Chunk: &types.Chunk{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000002"), DocumentID: docID, Content: "two"}, Score: 0.8},
	}}
	reranker := &fakeReranker{scores: []float64{1}}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docID}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, reranker, nil)

	_, err := retrieval.Retrieve(callerContext(uuid.New()), rerankRequest("who won?", types.RetrievalTechniqueDense))
	if err == nil || !strings.Contains(err.Error(), "1 scores") || !strings.Contains(err.Error(), "2 candidates") {
		t.Errorf("Retrieve = %v, want an error naming the score count and the candidate count", err)
	}
	if _, ok := apperrors.IsAppError(err); ok {
		t.Errorf("Retrieve = %v, want a plain error so the handler answers 500", err)
	}
}

func TestRetrieveWithRerankAndAnEmptyPoolSkipsTheCall(t *testing.T) {
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docID}}, &fakeChunkSearch{}, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, &forbiddenReranker{t: t}, nil)

	got, err := retrieval.Retrieve(callerContext(uuid.New()), rerankRequest("who won?", types.RetrievalTechniqueDense))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got.Results) != 0 {
		t.Errorf("results = %+v, want none", got.Results)
	}
	// The step is still recorded so the answer keeps its shape; no model was
	// called, so it names none.
	if len(got.Trace) != 3 {
		t.Fatalf("trace = %+v, want the embed, search and rerank steps", got.Trace)
	}
	step := got.Trace[2]
	if step.Step != types.TraceStepRerank || step.Technique != types.RetrievalTechniqueRerank ||
		step.Model != "" || step.Candidates == nil || *step.Candidates != 0 {
		t.Errorf("trace[2] = %+v, want the rerank step with 0 candidates and no model", step)
	}
}

func TestRetrieveParentChildCollapsesChunksOfOneParentChunk(t *testing.T) {
	principalID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a1")
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	parentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000c1")
	chunkA := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	chunkB := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")
	chunkStandalone := uuid.MustParse("0190d2a8-0000-7000-8000-000000000003")

	docs := &fakeReadableDocuments{readable: []uuid.UUID{docID}}
	chunks := &fakeChunkSearch{
		denseMatches: []*types.ChunkMatch{
			{Chunk: &types.Chunk{ID: chunkA, DocumentID: docID, Seq: 0, Content: "Điều 1. Phạm vi"}, ParentID: &parentID, Score: 0.9},
			{Chunk: &types.Chunk{ID: chunkStandalone, DocumentID: docID, Seq: 1, Content: "Điều 2. Đối tượng"}, Score: 0.7},
			{Chunk: &types.Chunk{ID: chunkB, DocumentID: docID, Seq: 2, Content: "Điều 3. Thu nhập"}, ParentID: &parentID, Score: 0.6},
		},
		parentChunks: []*types.ParentChunk{{
			ID: parentID, DocumentID: docID, Seq: 0,
			Content: "Điều 1. Phạm vi\nĐiều 3. Thu nhập", ContextHeader: "Chương I",
		}},
	}
	retrieval := NewRetrievalService(docs, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, nil, nil)

	req := parentChildRequest("who won?", types.RetrievalTechniqueDense)
	req.TopK = intPointer(5)
	got, err := retrieval.Retrieve(callerContext(principalID), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	// Collapsing can merge several Chunks into one result, so the search is
	// deeper than top_k even with a single technique.
	if chunks.denseTopK != types.RetrievalFusionCandidates {
		t.Errorf("dense top_k = %d, want the fusion depth %d", chunks.denseTopK, types.RetrievalFusionCandidates)
	}
	// The parent lookup runs once, over the readable set and the distinct
	// parent IDs, in rank order.
	if chunks.parentCalls != 1 {
		t.Fatalf("ListParentChunks called %d times, want once", chunks.parentCalls)
	}
	if !slices.Equal(chunks.parentDocumentIDs, []uuid.UUID{docID}) {
		t.Errorf("ListParentChunks document IDs = %v, want the readable set %v", chunks.parentDocumentIDs, []uuid.UUID{docID})
	}
	if !slices.Equal(chunks.parentIDs, []uuid.UUID{parentID}) {
		t.Errorf("ListParentChunks parent IDs = %v, want the distinct %v", chunks.parentIDs, []uuid.UUID{parentID})
	}

	if len(got.Results) != 2 {
		t.Fatalf("results = %+v, want the parent and the standalone Chunk", got.Results)
	}
	first := got.Results[0]
	if first.ChunkID != chunkA || first.DocumentID != docID || first.Seq != 0 {
		t.Errorf("first result ids = %s/%s/%d, want the best child %s", first.ChunkID, first.DocumentID, first.Seq, chunkA)
	}
	if first.Content != "Điều 1. Phạm vi\nĐiều 3. Thu nhập" || first.ContextHeader != "Chương I" {
		t.Errorf("first result content/header = %q/%q, want the Parent Chunk's", first.Content, first.ContextHeader)
	}
	if first.ParentChunkID == nil || *first.ParentChunkID != parentID {
		t.Errorf("first result parent_chunk_id = %v, want %s", first.ParentChunkID, parentID)
	}
	if first.Score != 0.9 || first.Scores[types.RetrievalTechniqueDense] != 0.9 ||
		first.Ranks[types.RetrievalTechniqueDense] != 1 {
		t.Errorf("first result score/scores/ranks = %v/%v/%v, want the best child's",
			first.Score, first.Scores, first.Ranks)
	}
	second := got.Results[1]
	if second.ChunkID != chunkStandalone || second.ParentChunkID != nil ||
		second.Content != "Điều 2. Đối tượng" || second.Score != 0.7 {
		t.Errorf("second result = %+v, want the standalone child unchanged", second)
	}

	// The expansion step comes last and names the children behind each
	// result in rank order.
	if len(got.Trace) != 3 {
		t.Fatalf("trace = %+v, want the embed, search and expand steps", got.Trace)
	}
	step := got.Trace[2]
	if step.Step != types.TraceStepExpandParents || step.Technique != types.RetrievalTechniqueParentChild ||
		step.DurationMS < 0 || step.Candidates == nil || *step.Candidates != 2 {
		t.Errorf("trace[2] = %+v, want the expand step with 2 results before the cut", step)
	}
	expanded := expandedEntries(t, step)
	if len(expanded) != 2 {
		t.Fatalf("expanded = %+v, want one entry per returned result", expanded)
	}
	if expansion := expanded[0]; expansion.ChunkID != chunkA || expansion.ParentChunkID == nil ||
		*expansion.ParentChunkID != parentID ||
		!slices.Equal(expansion.MatchedChunkIDs, []uuid.UUID{chunkA, chunkB}) {
		t.Errorf("expanded[0] = %+v, want chunk A over parent %s with the matched children in rank order",
			expansion, parentID)
	}
	if expansion := expanded[1]; expansion.ChunkID != chunkStandalone ||
		expansion.ParentChunkID != nil ||
		!slices.Equal(expansion.MatchedChunkIDs, []uuid.UUID{chunkStandalone}) {
		t.Errorf("expanded[1] = %+v, want the standalone child alone", expansion)
	}
}

func TestRetrieveParentChildStillFillsTopKAfterCollapse(t *testing.T) {
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	parentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000c1")
	chunkA := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	chunkB := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")
	chunkC := uuid.MustParse("0190d2a8-0000-7000-8000-000000000003")
	chunkD := uuid.MustParse("0190d2a8-0000-7000-8000-000000000004")

	chunks := &fakeChunkSearch{
		denseMatches: []*types.ChunkMatch{
			{Chunk: &types.Chunk{ID: chunkA, DocumentID: docID, Seq: 0, Content: "first"}, ParentID: &parentID, Score: 0.9},
			{Chunk: &types.Chunk{ID: chunkB, DocumentID: docID, Seq: 1, Content: "second"}, ParentID: &parentID, Score: 0.8},
			{Chunk: &types.Chunk{ID: chunkC, DocumentID: docID, Seq: 2, Content: "third"}, ParentID: &parentID, Score: 0.7},
			{Chunk: &types.Chunk{ID: chunkD, DocumentID: docID, Seq: 3, Content: "fourth"}, Score: 0.6},
		},
		parentChunks: []*types.ParentChunk{{
			ID: parentID, DocumentID: docID, Seq: 0, Content: "the three first Chunks",
		}},
	}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docID}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, nil, nil)

	req := parentChildRequest("who won?", types.RetrievalTechniqueDense)
	req.TopK = intPointer(2)
	got, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	// The first three children merge into the first result; the fourth fills
	// the second top_k place.
	if len(got.Results) != 2 || got.Results[1].ChunkID != chunkD {
		t.Fatalf("results = %+v, want the collapsed parent then chunk D", got.Results)
	}
	expanded := expandedEntries(t, got.Trace[len(got.Trace)-1])
	if len(expanded) != 2 ||
		!slices.Equal(expanded[0].MatchedChunkIDs, []uuid.UUID{chunkA, chunkB, chunkC}) {
		t.Errorf("expanded = %+v, want the three matched Chunks collapsed into the first result", expanded)
	}
}

func TestRetrieveParentChildFallsBackWhenAParentIsMissing(t *testing.T) {
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	parentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000c1")
	chunkID := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")

	chunks := &fakeChunkSearch{
		denseMatches: []*types.ChunkMatch{{
			Chunk:    &types.Chunk{ID: chunkID, DocumentID: docID, Content: "the child alone"},
			ParentID: &parentID,
			Score:    0.9,
		}},
	}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docID}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, nil, nil)

	req := parentChildRequest("who won?", types.RetrievalTechniqueDense)
	req.TopK = intPointer(5)
	got, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	// The Parent Chunk vanished between the search and the lookup, as a
	// re-ingest can make it; the child stands alone rather than disappearing.
	if len(got.Results) != 1 || got.Results[0].Content != "the child alone" ||
		got.Results[0].ParentChunkID != nil || got.Results[0].ChunkID != chunkID {
		t.Errorf("results = %+v, want the child unchanged with no parent_chunk_id", got.Results)
	}
	expanded := expandedEntries(t, got.Trace[len(got.Trace)-1])
	if len(expanded) != 1 || expanded[0].ParentChunkID != nil ||
		!slices.Equal(expanded[0].MatchedChunkIDs, []uuid.UUID{chunkID}) {
		t.Errorf("expanded = %+v, want the child as a fallback result", expanded)
	}
}

func TestRetrieveParentChildCollapsesOnTheRerankedOrderBeforeTheCut(t *testing.T) {
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	parentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000c1")
	chunkA := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	chunkB := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")
	chunkC := uuid.MustParse("0190d2a8-0000-7000-8000-000000000003")

	chunks := &fakeChunkSearch{
		denseMatches: []*types.ChunkMatch{
			{Chunk: &types.Chunk{ID: chunkA, DocumentID: docID, Seq: 0, Content: "near A"}, ParentID: &parentID, Score: 0.9},
			{Chunk: &types.Chunk{ID: chunkB, DocumentID: docID, Seq: 1, Content: "near B"}, ParentID: &parentID, Score: 0.8},
			{Chunk: &types.Chunk{ID: chunkC, DocumentID: docID, Seq: 2, Content: "far C"}, Score: 0.7},
		},
		parentChunks: []*types.ParentChunk{{
			ID: parentID, DocumentID: docID, Seq: 0, Content: "parent of A and B",
		}},
	}
	reranker := &fakeReranker{scores: []float64{0.8, 0.9, 0.7}, model: "fake-rerank"}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docID}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, reranker, nil)

	req := parentChildRequest("who won?", types.RetrievalTechniqueDense, types.RetrievalTechniqueRerank)
	req.TopK = intPointer(1)
	req.RetrievalConfig.RerankCandidates = intPointer(3)
	got, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	// With rerank on, the pool stays rerank_candidates even though collapsing
	// can merge results.
	if chunks.denseTopK != 3 {
		t.Errorf("dense top_k = %d, want the rerank pool 3", chunks.denseTopK)
	}
	if len(reranker.gotTexts) != 3 {
		t.Fatalf("reranked %d texts, want the whole pool of 3: %q", len(reranker.gotTexts), reranker.gotTexts)
	}
	// The collapse runs on the reranked order and only then cuts to top_k:
	// B leads, A shares its parent, and C is cut.
	if len(got.Results) != 1 {
		t.Fatalf("results = %+v, want the single top_k result", got.Results)
	}
	first := got.Results[0]
	if first.ChunkID != chunkB || first.Content != "parent of A and B" ||
		first.ParentChunkID == nil || *first.ParentChunkID != parentID {
		t.Errorf("first result = %+v, want B's parent", first)
	}
	if first.Score != 0.9 || first.Scores[types.RetrievalTechniqueDense] != 0.8 ||
		first.Ranks[types.RetrievalTechniqueRerank] != 1 || first.Ranks[types.RetrievalTechniqueDense] != 2 {
		t.Errorf("first result score/scores/ranks = %v/%v/%v, want B's rerank 0.9 over its dense 0.8",
			first.Score, first.Scores, first.Ranks)
	}

	// embed, search, rerank, expand: the expand step is last and reports the
	// two collapsed results before the cut.
	if len(got.Trace) != 4 {
		t.Fatalf("trace = %+v, want the embed, search, rerank and expand steps", got.Trace)
	}
	if step := got.Trace[2]; step.Step != types.TraceStepRerank {
		t.Errorf("trace[2] = %+v, want the rerank step before the expansion", step)
	}
	step := got.Trace[3]
	if step.Step != types.TraceStepExpandParents || step.Candidates == nil || *step.Candidates != 2 {
		t.Errorf("trace[3] = %+v, want the expand step with 2 results before the cut", step)
	}
	expanded := expandedEntries(t, step)
	if len(expanded) != 1 || expanded[0].ChunkID != chunkB ||
		!slices.Equal(expanded[0].MatchedChunkIDs, []uuid.UUID{chunkB, chunkA}) {
		t.Errorf("expanded = %+v, want B over A in the reranked order", expanded)
	}
}

func TestRetrieveParentChildSearchesAtLeastTheFusionDepth(t *testing.T) {
	chunks := &fakeChunkSearch{}
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, nil, nil)

	req := parentChildRequest("who won?", types.RetrievalTechniqueDense)
	req.TopK = intPointer(3)
	if _, err := retrieval.Retrieve(callerContext(uuid.New()), req); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if chunks.denseTopK != types.RetrievalFusionCandidates {
		t.Errorf("dense top_k = %d, want at least the fusion depth %d", chunks.denseTopK, types.RetrievalFusionCandidates)
	}

	req = parentChildRequest("who won?", types.RetrievalTechniqueDense)
	req.TopK = intPointer(types.RetrievalFusionCandidates + 10)
	if _, err := retrieval.Retrieve(callerContext(uuid.New()), req); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if chunks.denseTopK != types.RetrievalFusionCandidates+10 {
		t.Errorf("dense top_k = %d, want top_k above the fusion depth to be kept", chunks.denseTopK)
	}
}

func TestRetrieveParentChildWrapsAListParentsFailure(t *testing.T) {
	listErr := errors.New("postgres down")
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	parentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000c1")
	chunks := &fakeChunkSearch{
		denseMatches: []*types.ChunkMatch{{
			Chunk:    &types.Chunk{ID: uuid.New(), DocumentID: docID, Content: "near"},
			ParentID: &parentID,
			Score:    0.9,
		}},
		parentErr: listErr,
	}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docID}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, nil, nil)

	req := parentChildRequest("who won?", types.RetrievalTechniqueDense)
	req.TopK = intPointer(5)
	_, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if !errors.Is(err, listErr) || !strings.Contains(err.Error(), "parent chunks") {
		t.Errorf("Retrieve = %v, want the parent lookup error wrapped with what it was doing", err)
	}
	if _, ok := apperrors.IsAppError(err); ok {
		t.Errorf("Retrieve = %v, want a plain error so the handler answers 500", err)
	}
}

func TestRetrieveWithParentChildOffNeverListsParents(t *testing.T) {
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	parentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000c1")
	chunkID := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	chunks := &fakeChunkSearch{
		denseMatches: []*types.ChunkMatch{{
			Chunk:    &types.Chunk{ID: chunkID, DocumentID: docID, Content: "the child"},
			ParentID: &parentID,
			Score:    0.9,
		}},
		// A caller here would fail the request, so the assertion is strict.
		parentErr: errors.New("must not be reached"),
	}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{docID}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, nil, nil)

	got, err := retrieval.Retrieve(callerContext(uuid.New()), &types.RetrievalRequest{
		Query: "who won?", TopK: intPointer(5),
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if chunks.parentCalls != 0 {
		t.Errorf("ListParentChunks called %d times with parent_child off, want never", chunks.parentCalls)
	}
	if len(got.Results) != 1 || got.Results[0].Content != "the child" ||
		got.Results[0].ParentChunkID != nil {
		t.Errorf("results = %+v, want the child unchanged", got.Results)
	}
	for _, step := range got.Trace {
		if step.Step == types.TraceStepExpandParents {
			t.Errorf("trace = %+v, want no expand step with parent_child off", got.Trace)
		}
	}
}

func TestRetrieveParentChildWithNothingReadableKeepsTheEmptyExpandedList(t *testing.T) {
	chunks := &fakeChunkSearch{parentErr: errors.New("must not be reached")}
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{}}, chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"}, nil, nil)

	req := parentChildRequest("who won?", types.RetrievalTechniqueDense)
	req.TopK = intPointer(5)
	got, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if chunks.parentCalls != 0 {
		t.Errorf("ListParentChunks called %d times with nothing readable, want never", chunks.parentCalls)
	}

	step := got.Trace[len(got.Trace)-1]
	if step.Step != types.TraceStepExpandParents || step.Technique != types.RetrievalTechniqueParentChild ||
		step.Candidates == nil || *step.Candidates != 0 {
		t.Fatalf("trace last step = %+v, want expand_parents with 0 candidates", step)
	}
	if expanded := expandedEntries(t, step); len(expanded) != 0 {
		t.Errorf("expanded = %+v, want an empty list", expanded)
	}

	// The empty answer keeps its shape: expanded is present as [], not left
	// out the way an omitted slice would be.
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"expanded":[]`) {
		t.Errorf("JSON = %s, want the expand step's expanded as []", body)
	}
}

func TestRetrieveNeedsTheAuthenticatedPrincipal(t *testing.T) {
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, &fakeChunkSearch{}, &fakeEmbedder{}, nil, nil)

	_, err := retrieval.Retrieve(context.Background(), &types.RetrievalRequest{Query: "who won?"})
	if err == nil || !strings.Contains(err.Error(), "principal") {
		t.Errorf("Retrieve without a Principal = %v, want an error naming the missing Principal", err)
	}
}

// rewriteRequest runs bm25 with query_rewrite over history, the shape every
// query rewrite test needs.
func rewriteRequest(query string, history []types.ConversationTurn) *types.RetrievalRequest {
	return &types.RetrievalRequest{
		Query: query,
		TopK:  intPointer(5),
		RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{
			types.RetrievalTechniqueBM25, types.RetrievalTechniqueQueryRewrite,
		}},
		History: history,
	}
}

// oneRewriteHistory is one earlier turn, enough to make a follow-up a
// follow-up.
func oneRewriteHistory() []types.ConversationTurn {
	return []types.ConversationTurn{{
		Seq:      1,
		Question: "Thuế TNCN là gì?",
		Answer:   "Thuế TNCN là thuế thu nhập cá nhân.",
	}}
}

func TestRetrieveRewritesAFollowUpBeforeSearchingWithIt(t *testing.T) {
	principalID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a1")
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunkID := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")

	docs := &fakeReadableDocuments{readable: []uuid.UUID{docID}}
	chunks := &fakeChunkSearch{bm25Matches: []*types.ChunkMatch{
		{Chunk: &types.Chunk{ID: chunkID, DocumentID: docID, Seq: 0, Content: "Thuế TNCN năm 2024 giảm."}, Score: 2},
	}}
	usage := types.TokenUsage{PromptTokens: 21, CompletionTokens: 5, TotalTokens: 26}
	rewriter := &fakeGenerator{
		model: "test-chat",
		reply: &types.ChatResponse{Content: "  Thuế TNCN năm 2024  ", Usage: usage},
	}
	retrieval := NewRetrievalService(docs, chunks, nil, nil, rewriter)

	got, err := retrieval.Retrieve(callerContext(principalID), rewriteRequest("và năm 2024 thì sao?", oneRewriteHistory()))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if rewriter.calls != 1 {
		t.Fatalf("rewriter called %d times, want once", rewriter.calls)
	}
	if len(rewriter.gotTurns) != 2 || rewriter.gotTurns[0].Role != types.ChatRoleSystem {
		t.Fatalf("rewriter turns = %+v, want a system and a user turn", rewriter.gotTurns)
	}
	user := rewriter.gotTurns[1].Content
	for _, want := range []string{
		"Q: Thuế TNCN là gì?",
		"A: Thuế TNCN là thuế thu nhập cá nhân.",
		"Latest question: và năm 2024 thì sao?",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("rewrite user message %q does not carry %q", user, want)
		}
	}
	if strings.Index(user, "Q: Thuế") > strings.Index(user, "Latest question:") {
		t.Errorf("rewrite user message %q puts the history after the latest question", user)
	}

	if chunks.bm25Query != "Thuế TNCN năm 2024" {
		t.Errorf("bm25 searched %q, want the trimmed rewritten query", chunks.bm25Query)
	}
	if got.Query != "Thuế TNCN năm 2024" {
		t.Errorf("result query = %q, want the query retrieval ran with", got.Query)
	}
	if got.RewrittenQuery == nil || *got.RewrittenQuery != "Thuế TNCN năm 2024" {
		t.Errorf("result rewritten query = %v, want the trimmed rewritten query", got.RewrittenQuery)
	}
	if got.ChatUsage != usage {
		t.Errorf("result rewrite usage = %+v, want %+v", got.ChatUsage, usage)
	}
	if len(got.Trace) != 2 {
		t.Fatalf("trace = %+v, want the rewrite step then the bm25 search step", got.Trace)
	}
	step := got.Trace[0]
	if step.Step != types.TraceStepRewriteQuery || step.Technique != types.RetrievalTechniqueQueryRewrite ||
		step.Model != "test-chat" || step.RewrittenQuery != "Thuế TNCN năm 2024" ||
		step.Usage == nil || *step.Usage != usage || step.DurationMS < 0 {
		t.Errorf("trace[0] = %+v, want the rewrite_query step with the model, query and usage", step)
	}
	if step.Candidates != nil {
		t.Errorf("rewrite step candidates = %v, want none", *step.Candidates)
	}
	if got.Trace[1].Step != types.TraceStepSearch || got.Trace[1].Technique != types.RetrievalTechniqueBM25 {
		t.Errorf("trace[1] = %+v, want the bm25 search step after the rewrite", got.Trace[1])
	}
}

func TestRetrieveRewritesTheQueryForDenseToo(t *testing.T) {
	principalID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a1")
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunkID := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")

	docs := &fakeReadableDocuments{readable: []uuid.UUID{docID}}
	chunks := &fakeChunkSearch{denseMatches: []*types.ChunkMatch{
		{Chunk: &types.Chunk{ID: chunkID, DocumentID: docID, Seq: 0, Content: "Thuế TNCN năm 2024 giảm."}, Score: 0.9},
	}}
	embedder := &fakeEmbedder{vectors: [][]float32{{1, 0}}, model: "test-embedding"}
	rewriter := &fakeGenerator{reply: &types.ChatResponse{Content: "Thuế TNCN năm 2024"}}
	retrieval := NewRetrievalService(docs, chunks, embedder, nil, rewriter)

	req := rewriteRequest("và năm 2024 thì sao?", oneRewriteHistory())
	req.RetrievalConfig.Techniques = []types.RetrievalTechnique{
		types.RetrievalTechniqueDense, types.RetrievalTechniqueQueryRewrite,
	}
	if _, err := retrieval.Retrieve(callerContext(principalID), req); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if !slices.Equal(embedder.got, []string{"Thuế TNCN năm 2024"}) {
		t.Errorf("dense embedded %v, want the rewritten query", embedder.got)
	}
}

func TestRetrieveRewritesTheQueryForRerankToo(t *testing.T) {
	principalID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a1")
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunkID := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")

	docs := &fakeReadableDocuments{readable: []uuid.UUID{docID}}
	chunks := &fakeChunkSearch{bm25Matches: []*types.ChunkMatch{
		{Chunk: &types.Chunk{ID: chunkID, DocumentID: docID, Seq: 0, Content: "Thuế TNCN năm 2024 giảm."}, Score: 2},
	}}
	reranker := &fakeReranker{scores: []float64{0.9}, model: "test-rerank"}
	rewriter := &fakeGenerator{reply: &types.ChatResponse{Content: "Thuế TNCN năm 2024"}}
	retrieval := NewRetrievalService(docs, chunks, nil, reranker, rewriter)

	req := rewriteRequest("và năm 2024 thì sao?", oneRewriteHistory())
	req.RetrievalConfig.Techniques = []types.RetrievalTechnique{
		types.RetrievalTechniqueBM25, types.RetrievalTechniqueRerank, types.RetrievalTechniqueQueryRewrite,
	}
	if _, err := retrieval.Retrieve(callerContext(principalID), req); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if reranker.gotQuery != "Thuế TNCN năm 2024" {
		t.Errorf("rerank scored against %q, want the rewritten query", reranker.gotQuery)
	}
}

func TestRetrieveWithoutQueryRewriteSearchesTheRawQuestion(t *testing.T) {
	history := oneRewriteHistory()
	rewriter := &fakeGenerator{reply: &types.ChatResponse{Content: "must not be used"}}
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, &fakeChunkSearch{}, nil, nil, rewriter)

	req := rewriteRequest("và năm 2024 thì sao?", history)
	req.RetrievalConfig.Techniques = []types.RetrievalTechnique{types.RetrievalTechniqueBM25}
	got, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if rewriter.calls != 0 {
		t.Errorf("rewriter called %d times without query_rewrite, want none", rewriter.calls)
	}
	if got.Query != "và năm 2024 thì sao?" {
		t.Errorf("result query = %q, want the raw question", got.Query)
	}
	if got.RewrittenQuery != nil || got.ChatUsage != (types.TokenUsage{}) {
		t.Errorf("result rewrite = %v, %+v; want none without query_rewrite", got.RewrittenQuery, got.ChatUsage)
	}
	if len(got.Trace) != 1 || got.Trace[0].Step != types.TraceStepSearch {
		t.Errorf("trace = %+v, want only the search step", got.Trace)
	}
}

func TestRetrieveWithoutHistoryMakesNoRewriteCall(t *testing.T) {
	rewriter := &fakeGenerator{reply: &types.ChatResponse{Content: "must not be used"}}
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, &fakeChunkSearch{}, nil, nil, rewriter)

	got, err := retrieval.Retrieve(callerContext(uuid.New()), rewriteRequest("Thuế TNCN là gì?", nil))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if rewriter.calls != 0 {
		t.Errorf("rewriter called %d times without history, want none", rewriter.calls)
	}
	if got.Query != "Thuế TNCN là gì?" {
		t.Errorf("result query = %q, want the raw question", got.Query)
	}
	if got.RewrittenQuery != nil || got.ChatUsage != (types.TokenUsage{}) {
		t.Errorf("result rewrite = %v, %+v; want none without history", got.RewrittenQuery, got.ChatUsage)
	}
	if len(got.Trace) != 1 || got.Trace[0].Step != types.TraceStepSearch {
		t.Errorf("trace = %+v, want only the search step, no rewrite_query step", got.Trace)
	}
}

func TestRetrieveWithoutAChatModelCannotRewrite(t *testing.T) {
	docs := &fakeReadableDocuments{err: errors.New("must not be reached")}
	retrieval := NewRetrievalService(docs, &fakeChunkSearch{}, nil, nil, nil)

	_, err := retrieval.Retrieve(callerContext(uuid.New()), rewriteRequest("và năm 2024 thì sao?", oneRewriteHistory()))
	appErr, ok := apperrors.IsAppError(err)
	if !ok {
		t.Fatalf("Retrieve = %v, want an AppError", err)
	}
	if appErr.Code != apperrors.ErrServiceUnavailable || appErr.HTTPCode != http.StatusServiceUnavailable {
		t.Errorf("error code/status = %d/%d, want %d/503",
			appErr.Code, appErr.HTTPCode, apperrors.ErrServiceUnavailable)
	}
	if !strings.Contains(appErr.Message, "query rewrite") || !strings.Contains(appErr.Message, "models.chat") {
		t.Errorf("message %q does not name query rewrite and models.chat", appErr.Message)
	}
	if docs.calls != 0 {
		t.Errorf("readable Documents asked %d times without a chat model, want none", docs.calls)
	}
}

func TestRetrieveFallsBackToTheRawQuestionForABlankRewrite(t *testing.T) {
	usage := types.TokenUsage{PromptTokens: 4, CompletionTokens: 0, TotalTokens: 4}
	rewriter := &fakeGenerator{reply: &types.ChatResponse{Content: "   ", Usage: usage}}
	chunks := &fakeChunkSearch{}
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, chunks, nil, nil, rewriter)

	got, err := retrieval.Retrieve(callerContext(uuid.New()), rewriteRequest("và năm 2024 thì sao?", oneRewriteHistory()))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if chunks.bm25Query != "và năm 2024 thì sao?" {
		t.Errorf("bm25 searched %q, want the raw question after a blank reply", chunks.bm25Query)
	}
	if got.Query != "và năm 2024 thì sao?" {
		t.Errorf("result query = %q, want the raw question", got.Query)
	}
	if got.RewrittenQuery == nil || *got.RewrittenQuery != "và năm 2024 thì sao?" {
		t.Errorf("result rewritten query = %v, want the raw question after a blank reply", got.RewrittenQuery)
	}
	if got.ChatUsage != usage {
		t.Errorf("result rewrite usage = %+v, want the call's usage %+v even when it fell back", got.ChatUsage, usage)
	}
	step := got.Trace[0]
	if step.RewrittenQuery != "và năm 2024 thì sao?" {
		t.Errorf("rewritten_query = %q, want the query actually used", step.RewrittenQuery)
	}
	if step.Usage == nil || *step.Usage != usage {
		t.Errorf("usage = %v, want the call's usage even when it fell back", step.Usage)
	}
}

func TestRetrieveFallsBackToTheRawQuestionForAnOverlongRewrite(t *testing.T) {
	rewriter := &fakeGenerator{reply: &types.ChatResponse{
		Content: strings.Repeat("a", types.RetrievalQueryMaxRunes+1),
	}}
	chunks := &fakeChunkSearch{}
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, chunks, nil, nil, rewriter)

	got, err := retrieval.Retrieve(callerContext(uuid.New()), rewriteRequest("và năm 2024 thì sao?", oneRewriteHistory()))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if chunks.bm25Query != "và năm 2024 thì sao?" {
		t.Errorf("bm25 searched %q, want the raw question after an over-long reply", chunks.bm25Query)
	}
	if got.Trace[0].RewrittenQuery != "và năm 2024 thì sao?" {
		t.Errorf("rewritten_query = %q, want the query actually used", got.Trace[0].RewrittenQuery)
	}
	if got.RewrittenQuery == nil || *got.RewrittenQuery != "và năm 2024 thì sao?" {
		t.Errorf("result rewritten query = %v, want the raw question after an over-long reply", got.RewrittenQuery)
	}
}

func TestRewritePromptCutsALongHistoryAnswer(t *testing.T) {
	answer := strings.Repeat("x", rewriteAnswerMaxRunes+200)
	rewriter := &fakeGenerator{reply: &types.ChatResponse{Content: "rewritten"}}
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, &fakeChunkSearch{}, nil, nil, rewriter)

	history := []types.ConversationTurn{{Seq: 1, Question: "q", Answer: answer}}
	if _, err := retrieval.Retrieve(callerContext(uuid.New()), rewriteRequest("follow-up", history)); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(rewriter.gotTurns) != 2 {
		t.Fatalf("rewriter turns = %+v, want a system and a user turn", rewriter.gotTurns)
	}
	prompt := rewriter.gotTurns[1].Content
	if !strings.Contains(prompt, strings.Repeat("x", rewriteAnswerMaxRunes)+"…") {
		t.Errorf("rewrite prompt does not carry the answer cut to %d runes with an ellipsis", rewriteAnswerMaxRunes)
	}
	if strings.Contains(prompt, strings.Repeat("x", rewriteAnswerMaxRunes+1)) {
		t.Errorf("rewrite prompt carries more than %d runes of the answer", rewriteAnswerMaxRunes)
	}
}

func TestRetrieveKeepsAProviderRejectionOfARewrite(t *testing.T) {
	cause := errors.New("the provider said secret")
	rewriter := &fakeGenerator{err: apperrors.NewModelProviderError("chat", http.StatusTooManyRequests, cause)}
	docs := &fakeReadableDocuments{err: errors.New("must not be reached")}
	retrieval := NewRetrievalService(docs, &fakeChunkSearch{}, nil, nil, rewriter)

	_, err := retrieval.Retrieve(callerContext(uuid.New()), rewriteRequest("và năm 2024 thì sao?", oneRewriteHistory()))
	appErr, ok := apperrors.IsAppError(err)
	if !ok || appErr.Code != apperrors.ErrModelProvider || appErr.HTTPCode != http.StatusBadGateway {
		t.Fatalf("Retrieve = %v, want the provider-rejection 502", err)
	}
	if !strings.Contains(appErr.Message, "chat") {
		t.Errorf("message = %q, want it to name the chat role", appErr.Message)
	}
	if strings.Contains(appErr.Message, "secret") {
		t.Errorf("message = %q, want the provider's body left out", appErr.Message)
	}
	if docs.calls != 0 {
		t.Errorf("readable Documents asked %d times after a failed rewrite, want none", docs.calls)
	}
}

func TestParseSubQuestionsKeepsTheUsableEntries(t *testing.T) {
	long := strings.Repeat("a", types.RetrievalQueryMaxRunes+1)
	for name, tc := range map[string]struct {
		reply string
		query string
		want  []string
	}{
		"json array": {
			reply: `["Who wrote the plan?", "When was it approved?"]`,
			query: "Who wrote the plan and when was it approved?",
			want:  []string{"Who wrote the plan?", "When was it approved?"},
		},
		"fenced json array": {
			reply: "```json\n[\"Who wrote the plan?\", \"When was it approved?\"]\n```",
			query: "Who wrote the plan and when was it approved?",
			want:  []string{"Who wrote the plan?", "When was it approved?"},
		},
		"numbered lines": {
			reply: "1. Who wrote the plan?\n2) When was it approved?",
			query: "Who wrote the plan and when was it approved?",
			want:  []string{"Who wrote the plan?", "When was it approved?"},
		},
		"bulleted lines": {
			reply: "- Who wrote the plan?\n* When was it approved?",
			query: "Who wrote the plan and when was it approved?",
			want:  []string{"Who wrote the plan?", "When was it approved?"},
		},
		"blank, over-long and duplicate entries dropped": {
			reply: `["  Who wrote the plan?  ", "", "` + long + `", "who WROTE the plan?", "When was it approved?", "When was it approved?"]`,
			query: "Who wrote the plan and when was it approved?",
			want:  []string{"Who wrote the plan?", "When was it approved?"},
		},
		"entry equal to the original dropped": {
			reply: `["  who won?  ", "Who came second?"]`,
			query: "who won?",
			want:  []string{"Who came second?"},
		},
		"capped at four": {
			reply: `["One?", "Two?", "Three?", "Four?", "Five?"]`,
			query: "original",
			want:  []string{"One?", "Two?", "Three?", "Four?"},
		},
		"one usable entry stays below two": {
			reply: `["Only one?", "only ONE?", "  "]`,
			query: "original",
			want:  []string{"Only one?"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := parseSubQuestions(tc.reply, tc.query)
			if !slices.Equal(got, tc.want) {
				t.Errorf("parseSubQuestions(%q) = %q, want %q", tc.reply, got, tc.want)
			}
		})
	}

	// The over-long entry really is over the limit, so the case above tests
	// the drop rather than a shorter string.
	if len(long) <= types.RetrievalQueryMaxRunes {
		t.Fatalf("test setup: %q has %d runes, want over %d", long, len(long), types.RetrievalQueryMaxRunes)
	}
}

func TestParseSubQuestionsAnswersNoEntryForAnUnusableReply(t *testing.T) {
	for name, reply := range map[string]string{
		"blank":       "   \n\t",
		"empty array": `[]`,
	} {
		if got := parseSubQuestions(reply, "original"); len(got) != 0 {
			t.Errorf("%s: parseSubQuestions(%q) = %q, want no sub-question", name, reply, got)
		}
	}
}

// decompositionReply is the chat reply a decompose prompt asks for: a JSON
// array of sub-questions.
func decompositionReply(t *testing.T, subQuestions ...string) *types.ChatResponse {
	t.Helper()
	body, err := json.Marshal(subQuestions)
	if err != nil {
		t.Fatal(err)
	}
	return &types.ChatResponse{Content: string(body)}
}

// decomposeRequest runs the named candidate techniques with
// query_decomposition switched on.
func decomposeRequest(query string, techniques ...types.RetrievalTechnique) *types.RetrievalRequest {
	return &types.RetrievalRequest{
		Query: query,
		TopK:  intPointer(5),
		RetrievalConfig: &types.RetrievalConfig{
			Techniques: append(slices.Clone(techniques), types.RetrievalTechniqueQueryDecomposition),
		},
	}
}

// chunkMatch is one Chunk a fake technique found, for a per-query list.
func chunkMatch(chunkID, documentID uuid.UUID, content string, score float64) *types.ChunkMatch {
	return &types.ChunkMatch{
		Chunk: &types.Chunk{ID: chunkID, DocumentID: documentID, Content: content},
		Score: score,
	}
}

func TestRetrieveDecomposesTheQueryForEveryTechnique(t *testing.T) {
	principalID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a1")
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunkOriginal := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	chunkFirst := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")
	chunkSecond := uuid.MustParse("0190d2a8-0000-7000-8000-000000000003")
	chunkDense := uuid.MustParse("0190d2a8-0000-7000-8000-000000000004")

	const (
		query  = "Who wrote the plan and when was it approved?"
		first  = "Who wrote the plan?"
		second = "When was it approved?"
	)
	docs := &fakeReadableDocuments{readable: []uuid.UUID{docID}}
	chunks := &fakeChunkSearch{
		denseMatches: []*types.ChunkMatch{chunkMatch(chunkDense, docID, "dense match", 0.9)},
		bm25MatchesByQuery: map[string][]*types.ChunkMatch{
			query:  {chunkMatch(chunkOriginal, docID, "original match", 3)},
			first:  {chunkMatch(chunkFirst, docID, "first match", 2)},
			second: {chunkMatch(chunkSecond, docID, "second match", 1)},
		},
	}
	embedder := &fakeEmbedder{vectors: [][]float32{{0.1, 0.2}}, model: "test-embedding"}
	chat := &fakeGenerator{model: "test-chat", reply: decompositionReply(t, first, second)}
	retrieval := NewRetrievalService(docs, chunks, embedder, nil, chat)

	got, err := retrieval.Retrieve(callerContext(principalID),
		decomposeRequest(query, types.RetrievalTechniqueDense, types.RetrievalTechniqueBM25))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if chat.calls != 1 {
		t.Fatalf("chat called %d times, want one decomposition", chat.calls)
	}
	if len(chat.gotTurns) != 2 || chat.gotTurns[0].Role != types.ChatRoleSystem ||
		chat.gotTurns[1].Content != query {
		t.Errorf("decompose turns = %+v, want the system prompt and the original question", chat.gotTurns)
	}

	// The original and both sub-questions reach the searcher and the
	// embedder, in any order because the runs are concurrent.
	wantQueries := []string{query, first, second}
	bm25Queries := slices.Clone(chunks.bm25Queries)
	slices.Sort(bm25Queries)
	slices.Sort(wantQueries)
	if !slices.Equal(bm25Queries, wantQueries) {
		t.Errorf("bm25 searched %q, want the original and both sub-questions %q", bm25Queries, wantQueries)
	}
	embedded := embedder.embeddedTexts()
	slices.Sort(embedded)
	if !slices.Equal(embedded, wantQueries) {
		t.Errorf("dense embedded %q, want the original and both sub-questions %q", embedded, wantQueries)
	}
	if chunks.bm25Calls != 3 || chunks.denseCalls != 3 || embedder.calls != 3 {
		t.Errorf("bm25/dense/embed calls = %d/%d/%d, want 3 each for three queries",
			chunks.bm25Calls, chunks.denseCalls, embedder.calls)
	}

	// The trace starts with the decomposition, then the steps in query index
	// order and technique request order, then one fuse.
	wantSteps := []struct {
		step       string
		technique  types.RetrievalTechnique
		queryIndex int
	}{
		{types.TraceStepEmbedQuery, types.RetrievalTechniqueDense, 0},
		{types.TraceStepSearch, types.RetrievalTechniqueDense, 0},
		{types.TraceStepSearch, types.RetrievalTechniqueBM25, 0},
		{types.TraceStepEmbedQuery, types.RetrievalTechniqueDense, 1},
		{types.TraceStepSearch, types.RetrievalTechniqueDense, 1},
		{types.TraceStepSearch, types.RetrievalTechniqueBM25, 1},
		{types.TraceStepEmbedQuery, types.RetrievalTechniqueDense, 2},
		{types.TraceStepSearch, types.RetrievalTechniqueDense, 2},
		{types.TraceStepSearch, types.RetrievalTechniqueBM25, 2},
	}
	if len(got.Trace) != len(wantSteps)+2 {
		t.Fatalf("trace = %+v, want the decompose step, %d search steps and the fuse", got.Trace, len(wantSteps))
	}
	decompose := got.Trace[0]
	if decompose.Step != types.TraceStepDecomposeQuery ||
		decompose.Technique != types.RetrievalTechniqueQueryDecomposition ||
		decompose.Model != "test-chat" || decompose.Queries == nil || decompose.DurationMS < 0 {
		t.Errorf("trace[0] = %+v, want the decompose_query step naming test-chat with its queries", decompose)
	}
	for i, want := range wantSteps {
		step := got.Trace[i+1]
		if step.Step != want.step || step.Technique != want.technique ||
			step.QueryIndex == nil || *step.QueryIndex != want.queryIndex {
			t.Errorf("trace[%d] = %+v, want %s/%s with query_index %d",
				i+1, step, want.step, want.technique, want.queryIndex)
		}
	}
	if last := got.Trace[len(got.Trace)-1]; last.Step != types.TraceStepFuse {
		t.Errorf("trace ends with %+v, want the fuse step", last)
	}
	if got.Query != query {
		t.Errorf("result query = %q, want the original question that generation answers", got.Query)
	}
}

func TestRetrieveRecordsEachQueryEvidenceInTheDecomposeStep(t *testing.T) {
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunkOne := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	chunkTwo := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")
	chunkThree := uuid.MustParse("0190d2a8-0000-7000-8000-000000000003")
	chunkFour := uuid.MustParse("0190d2a8-0000-7000-8000-000000000004")

	const (
		query  = "original question"
		first  = "first part?"
		second = "second part?"
	)
	chunks := &fakeChunkSearch{bm25MatchesByQuery: map[string][]*types.ChunkMatch{
		query: {
			chunkMatch(chunkOne, docID, "one", 3),
			chunkMatch(chunkTwo, docID, "two", 2),
			chunkMatch(chunkThree, docID, "three", 1),
		},
		first: {chunkMatch(chunkFour, docID, "four", 1)},
	}}
	chat := &fakeGenerator{reply: decompositionReply(t, first, second)}
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, chunks, nil, nil, chat)

	req := decomposeRequest(query, types.RetrievalTechniqueBM25)
	req.TopK = intPointer(2)
	got, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if got.Trace[0].Queries == nil {
		t.Fatal("decompose step left queries out, want one entry per query")
	}
	want := []types.QueryEvidence{
		{Query: query, ChunkIDs: []uuid.UUID{chunkOne, chunkTwo}},
		{Query: first, ChunkIDs: []uuid.UUID{chunkFour}},
		{Query: second, ChunkIDs: []uuid.UUID{}},
	}
	if evidence := *got.Trace[0].Queries; !reflect.DeepEqual(evidence, want) {
		t.Errorf("queries = %+v, want each query's own matches cut to top_k %+v", evidence, want)
	}

	// More than one list makes every technique search the fusion depth, even
	// the one technique this request names.
	wantDepth := max(*req.TopK, types.RetrievalFusionCandidates)
	if len(chunks.bm25TopKs) != len(want) {
		t.Fatalf("bm25 searched %d times, want one per query", len(chunks.bm25TopKs))
	}
	for i, depth := range chunks.bm25TopKs {
		if depth != wantDepth {
			t.Errorf("bm25 search %d depth = %d, want the fusion depth %d with more than one list",
				i, depth, wantDepth)
		}
	}

	wantCandidates := []int{3, 1, 0}
	if len(got.Trace) != 1+len(wantCandidates)+1 {
		t.Fatalf("trace = %+v, want the decompose step, three searches and the fuse", got.Trace)
	}
	for i, candidates := range wantCandidates {
		step := got.Trace[i+1]
		if step.Step != types.TraceStepSearch || step.Technique != types.RetrievalTechniqueBM25 ||
			step.QueryIndex == nil || *step.QueryIndex != i || step.Candidates == nil || *step.Candidates != candidates {
			t.Errorf("trace[%d] = %+v, want the bm25 search for query_index %d with %d candidates",
				i+1, step, i, candidates)
		}
	}

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"chunk_ids":[]`) {
		t.Errorf("JSON = %s, want the query with no match to carry chunk_ids: []", body)
	}
	if !strings.Contains(string(body), `"query_index":0`) {
		t.Errorf("JSON = %s, want the search steps to carry their query index", body)
	}
}

func TestRetrieveSearchesOnlyTheOriginalWhenFewerThanTwoSubQuestionsRemain(t *testing.T) {
	const query = "original question"
	chunkID := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunks := &fakeChunkSearch{bm25Matches: []*types.ChunkMatch{chunkMatch(chunkID, docID, "only", 1)}}
	chat := &fakeGenerator{reply: decompositionReply(t, "  Only one?  ", "only ONE?", "")}
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, chunks, nil, nil, chat)

	req := decomposeRequest(query, types.RetrievalTechniqueBM25)
	req.TopK = intPointer(2)
	got, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if !slices.Equal(chunks.bm25Queries, []string{query}) {
		t.Errorf("bm25 searched %q, want only the original", chunks.bm25Queries)
	}
	// One list keeps the plain top_k depth, not the fusion depth.
	if !slices.Equal(chunks.bm25TopKs, []int{*req.TopK}) {
		t.Errorf("bm25 searched %v deep, want exactly top_k %d with a single list", chunks.bm25TopKs, *req.TopK)
	}
	if len(got.Trace) != 2 {
		t.Fatalf("trace = %+v, want the decompose step then the original's search", got.Trace)
	}
	decompose := got.Trace[0]
	if decompose.Step != types.TraceStepDecomposeQuery || decompose.Queries == nil {
		t.Fatalf("trace[0] = %+v, want the decompose step even when only the original runs", decompose)
	}
	want := []types.QueryEvidence{{Query: query, ChunkIDs: []uuid.UUID{chunkID}}}
	if evidence := *decompose.Queries; !reflect.DeepEqual(evidence, want) {
		t.Errorf("queries = %+v, want only the original %+v", evidence, want)
	}
	step := got.Trace[1]
	if step.Step != types.TraceStepSearch || step.QueryIndex == nil || *step.QueryIndex != 0 {
		t.Errorf("trace[1] = %+v, want the original's search carrying query_index 0", step)
	}
	for _, step := range got.Trace {
		if step.Step == types.TraceStepFuse {
			t.Errorf("trace has a fuse step %+v with a single list", step)
		}
	}
}

func TestRetrieveWithoutDecompositionMakesNoCallAndLeavesQueryIndexOut(t *testing.T) {
	chat := &fakeGenerator{reply: decompositionReply(t, "must not be used?")}
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, &fakeChunkSearch{}, nil, nil, chat)

	req := &types.RetrievalRequest{
		Query:           "who won?",
		RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{types.RetrievalTechniqueBM25}},
	}
	got, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if chat.calls != 0 {
		t.Errorf("chat called %d times without query_decomposition, want none", chat.calls)
	}
	if len(got.Trace) != 1 || got.Trace[0].Step != types.TraceStepSearch || got.Trace[0].QueryIndex != nil {
		t.Errorf("trace = %+v, want the search step alone without a query_index", got.Trace)
	}
	body, err := json.Marshal(got.Trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "query_index") || strings.Contains(string(body), `"queries"`) ||
		strings.Contains(string(body), "decompose_query") {
		t.Errorf("JSON = %s, want today's trace unchanged", body)
	}
}

func TestRetrieveWithoutAChatModelCannotDecompose(t *testing.T) {
	docs := &fakeReadableDocuments{err: errors.New("must not be reached")}
	retrieval := NewRetrievalService(docs, &fakeChunkSearch{}, nil, nil, nil)

	_, err := retrieval.Retrieve(callerContext(uuid.New()),
		decomposeRequest("who won and when?", types.RetrievalTechniqueBM25))
	appErr, ok := apperrors.IsAppError(err)
	if !ok {
		t.Fatalf("Retrieve = %v, want an AppError", err)
	}
	if appErr.Code != apperrors.ErrServiceUnavailable || appErr.HTTPCode != http.StatusServiceUnavailable {
		t.Errorf("error code/status = %d/%d, want %d/503",
			appErr.Code, appErr.HTTPCode, apperrors.ErrServiceUnavailable)
	}
	if !strings.Contains(appErr.Message, "query decomposition") || !strings.Contains(appErr.Message, "models.chat") {
		t.Errorf("message %q does not name query decomposition and models.chat", appErr.Message)
	}
	if docs.calls != 0 {
		t.Errorf("readable Documents asked %d times without a chat model, want none", docs.calls)
	}
}

func TestRetrieveReranksAgainstTheOriginalQueryNotASubQuestion(t *testing.T) {
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunkOriginal := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	chunkFirst := uuid.MustParse("0190d2a8-0000-7000-8000-000000000002")
	chunkSecond := uuid.MustParse("0190d2a8-0000-7000-8000-000000000003")

	const (
		query  = "original question"
		first  = "first part?"
		second = "second part?"
	)
	chunks := &fakeChunkSearch{bm25MatchesByQuery: map[string][]*types.ChunkMatch{
		query:  {chunkMatch(chunkOriginal, docID, "original", 3)},
		first:  {chunkMatch(chunkFirst, docID, "first", 2)},
		second: {chunkMatch(chunkSecond, docID, "second", 1)},
	}}
	reranker := &fakeReranker{scores: []float64{0.1, 0.9, 0.5}, model: "test-rerank"}
	chat := &fakeGenerator{reply: decompositionReply(t, first, second)}
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, chunks, nil, reranker, chat)

	got, err := retrieval.Retrieve(callerContext(uuid.New()),
		decomposeRequest(query, types.RetrievalTechniqueBM25, types.RetrievalTechniqueRerank))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if reranker.calls != 1 {
		t.Fatalf("reranker called %d times, want once", reranker.calls)
	}
	if reranker.gotQuery != query {
		t.Errorf("rerank scored against %q, want the original query", reranker.gotQuery)
	}
	if len(reranker.gotTexts) != 3 {
		t.Errorf("rerank scored %d texts, want one per distinct matched Chunk", len(reranker.gotTexts))
	}
	if last := got.Trace[len(got.Trace)-1]; last.Step != types.TraceStepRerank {
		t.Errorf("trace ends with %+v, want the rerank step", last)
	}
	if got.Query != query {
		t.Errorf("result query = %q, want the original question", got.Query)
	}
}

func TestRetrieveDecomposesTheRewrittenQueryAndSumsTheChatUsage(t *testing.T) {
	docID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunkID := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	rewriteUsage := types.TokenUsage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14}
	decomposeUsage := types.TokenUsage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25}
	const (
		rewritten = "standalone query"
		first     = "part one?"
		second    = "part two?"
	)
	chat := &fakeGenerator{model: "test-chat", replies: []*types.ChatResponse{
		{Content: "  " + rewritten + "  ", Usage: rewriteUsage},
		{Content: `["part one?", "part two?"]`, Usage: decomposeUsage},
	}}
	chunks := &fakeChunkSearch{
		bm25MatchesByQuery: map[string][]*types.ChunkMatch{
			rewritten: {chunkMatch(chunkID, docID, "rewritten match", 1)},
		},
	}
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, chunks, nil, nil, chat)

	req := rewriteRequest("và năm 2024 thì sao?", oneRewriteHistory())
	req.RetrievalConfig.Techniques = append(req.RetrievalConfig.Techniques,
		types.RetrievalTechniqueQueryDecomposition)
	got, err := retrieval.Retrieve(callerContext(uuid.New()), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if chat.calls != 2 {
		t.Fatalf("chat called %d times, want the rewrite then the decomposition", chat.calls)
	}
	if len(chat.allTurns) != 2 || len(chat.allTurns[1]) != 2 || chat.allTurns[1][1].Content != rewritten {
		t.Errorf("decompose turns = %+v, want the rewritten query %q", chat.allTurns, rewritten)
	}
	wantQueries := []string{first, rewritten, second}
	queries := slices.Clone(chunks.bm25Queries)
	slices.Sort(queries)
	slices.Sort(wantQueries)
	if !slices.Equal(queries, wantQueries) {
		t.Errorf("bm25 searched %q, want the rewritten query and both sub-questions %q", queries, wantQueries)
	}
	if len(got.Trace) < 2 || got.Trace[0].Step != types.TraceStepRewriteQuery ||
		got.Trace[1].Step != types.TraceStepDecomposeQuery {
		t.Fatalf("trace = %+v, want the rewrite then the decompose step", got.Trace)
	}
	if got.Trace[0].Usage == nil || *got.Trace[0].Usage != rewriteUsage {
		t.Errorf("rewrite usage = %v, want %+v", got.Trace[0].Usage, rewriteUsage)
	}
	if got.Trace[1].Usage == nil || *got.Trace[1].Usage != decomposeUsage {
		t.Errorf("decompose usage = %v, want %+v", got.Trace[1].Usage, decomposeUsage)
	}
	decompose := got.Trace[1]
	if decompose.Queries == nil || (*decompose.Queries)[0].Query != rewritten {
		t.Errorf("queries = %+v, want entry 0 to be the rewritten query %q", decompose.Queries, rewritten)
	}
	if got.Query != rewritten || got.RewrittenQuery == nil || *got.RewrittenQuery != rewritten {
		t.Errorf("result query/rewritten = %q/%v, want the rewritten query", got.Query, got.RewrittenQuery)
	}
	wantUsage := types.TokenUsage{PromptTokens: 30, CompletionTokens: 9, TotalTokens: 39}
	if got.ChatUsage != wantUsage {
		t.Errorf("chat usage = %+v, want the rewrite and decompose calls summed to %+v", got.ChatUsage, wantUsage)
	}
}
