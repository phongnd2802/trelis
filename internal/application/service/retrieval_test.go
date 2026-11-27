package service

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
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
// fixed matches. The two techniques run concurrently, so each records into
// its own fields instead of sharing them.
type fakeChunkSearch struct {
	interfaces.ChunkSearcher
	denseMatches []*types.ChunkMatch
	bm25Matches  []*types.ChunkMatch
	denseErr     error
	bm25Err      error
	denseCalls   int
	bm25Calls    int

	denseDocumentIDs []uuid.UUID
	denseVector      []float32
	denseModel       string
	denseTopK        int

	bm25DocumentIDs []uuid.UUID
	bm25Query       string
	bm25TopK        int
}

func (f *fakeChunkSearch) SearchDense(_ context.Context, documentIDs []uuid.UUID, queryVector []float32, model string, topK int) ([]*types.ChunkMatch, error) {
	f.denseCalls++
	f.denseDocumentIDs = append([]uuid.UUID(nil), documentIDs...)
	f.denseVector = append([]float32(nil), queryVector...)
	f.denseModel, f.denseTopK = model, topK
	return f.denseMatches, f.denseErr
}

func (f *fakeChunkSearch) SearchBM25(_ context.Context, documentIDs []uuid.UUID, query string, topK int) ([]*types.ChunkMatch, error) {
	f.bm25Calls++
	f.bm25DocumentIDs = append([]uuid.UUID(nil), documentIDs...)
	f.bm25Query, f.bm25TopK = query, topK
	return f.bm25Matches, f.bm25Err
}

// fakeEmbedder answers one vector per text and records what it was asked to
// embed.
type fakeEmbedder struct {
	vectors [][]float32
	err     error
	model   string
	calls   int
	got     []string
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	f.got = append([]string(nil), texts...)
	return f.vectors, f.err
}

func (f *fakeEmbedder) GetModelName() string { return f.model }

// callerContext is ctx with a Principal, as the router's middleware leaves it.
func callerContext(principalID uuid.UUID) context.Context {
	return types.WithPrincipal(context.Background(), types.Principal{ID: principalID, Name: "alice"})
}

// intPointer returns a pointer to n, for an explicitly given top_k.
func intPointer(n int) *int { return &n }

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

// TestEveryKnownTechniqueHasARunner guards the runner map from drifting away
// from the techniques validation admits.
func TestEveryKnownTechniqueHasARunner(t *testing.T) {
	retrieval, ok := NewRetrievalService(nil, nil, nil).(*retrievalService)
	if !ok {
		t.Fatal("NewRetrievalService did not return a *retrievalService")
	}
	for _, technique := range types.KnownRetrievalTechniques() {
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
	retrieval := NewRetrievalService(docs, chunks, embedder)

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
	retrieval := NewRetrievalService(docs, chunks, embedder)

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
	retrieval := NewRetrievalService(docs, chunks, nil)

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
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{uuid.New()}}, chunks, nil)

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
	retrieval := NewRetrievalService(docs, &fakeChunkSearch{}, embedder)

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
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{uuid.New()}}, &fakeChunkSearch{}, embedder)

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
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{uuid.New()}},
		chunks, &fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"})

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
		"unknown technique": {Query: "who won?", RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{"rerank"}}},
		"duplicate":         {Query: "who won?", RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{types.RetrievalTechniqueDense, types.RetrievalTechniqueDense}}},
		"rrf_k zero":        {Query: "who won?", RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{types.RetrievalTechniqueBM25}, RRFK: intPointer(0)}},
	} {
		t.Run(name, func(t *testing.T) {
			docs := &fakeReadableDocuments{err: errors.New("must not be reached")}
			chunks := &fakeChunkSearch{}
			embedder := &fakeEmbedder{}
			retrieval := NewRetrievalService(docs, chunks, embedder)

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
	lists := map[types.RetrievalTechnique][]*types.ChunkMatch{
		types.RetrievalTechniqueBM25: {
			{Chunk: chunkA, Score: 5},
			{Chunk: chunkB, Score: 3},
		},
		types.RetrievalTechniqueDense: {
			{Chunk: chunkB, Score: 0.9},
			{Chunk: chunkC, Score: 0.8},
		},
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
	got, _ := fuseRRF(map[types.RetrievalTechnique][]*types.ChunkMatch{
		types.RetrievalTechniqueBM25:  {{Chunk: chunkB, Score: 4}},
		types.RetrievalTechniqueDense: {{Chunk: chunkA, Score: 0.9}},
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
	got, distinct := fuseRRF(map[types.RetrievalTechnique][]*types.ChunkMatch{
		types.RetrievalTechniqueBM25: {
			{Chunk: chunks[0], Score: 4},
			{Chunk: chunks[1], Score: 3},
			{Chunk: chunks[2], Score: 2},
		},
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
	retrieval := NewRetrievalService(docs, chunks, embedder)

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
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, chunks,
		&fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"})

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
	retrieval := NewRetrievalService(docs, &fakeChunkSearch{}, nil)

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
	retrieval := NewRetrievalService(&fakeReadableDocuments{readable: []uuid.UUID{uuid.New()}}, chunks,
		&fakeEmbedder{vectors: [][]float32{{1}}, model: "fake-embedding"})

	_, err := retrieval.Retrieve(callerContext(uuid.New()), hybridRequest("who won?"))
	if !errors.Is(err, searchErr) || !strings.Contains(err.Error(), "bm25") {
		t.Errorf("Retrieve = %v, want the BM25 search error wrapped with what it was doing", err)
	}
}

func TestRetrieveNeedsTheAuthenticatedPrincipal(t *testing.T) {
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, &fakeChunkSearch{}, &fakeEmbedder{})

	_, err := retrieval.Retrieve(context.Background(), &types.RetrievalRequest{Query: "who won?"})
	if err == nil || !strings.Contains(err.Error(), "principal") {
		t.Errorf("Retrieve without a Principal = %v, want an error naming the missing Principal", err)
	}
}
