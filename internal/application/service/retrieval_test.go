package service

import (
	"context"
	"errors"
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

// fakeChunkSearch records the dense search arguments and answers fixed
// matches.
type fakeChunkSearch struct {
	interfaces.ChunkSearcher
	matches        []*types.ChunkMatch
	err            error
	calls          int
	gotDocumentIDs []uuid.UUID
	gotVector      []float32
	gotModel       string
	gotTopK        int
}

func (f *fakeChunkSearch) SearchDense(_ context.Context, documentIDs []uuid.UUID, queryVector []float32, model string, topK int) ([]*types.ChunkMatch, error) {
	f.calls++
	f.gotDocumentIDs = append([]uuid.UUID(nil), documentIDs...)
	f.gotVector = append([]float32(nil), queryVector...)
	f.gotModel, f.gotTopK = model, topK
	return f.matches, f.err
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
	chunks := &fakeChunkSearch{matches: []*types.ChunkMatch{
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
	if chunks.calls != 1 {
		t.Fatalf("search called %d times, want once", chunks.calls)
	}
	if !slices.Equal(chunks.gotDocumentIDs, []uuid.UUID{docA, docB}) {
		t.Errorf("search document IDs = %v, want the readable set %v", chunks.gotDocumentIDs, []uuid.UUID{docA, docB})
	}
	if !slices.Equal(chunks.gotVector, []float32{0.1, 0.2}) {
		t.Errorf("search vector = %v, want the embedded query %v", chunks.gotVector, []float32{0.1, 0.2})
	}
	if chunks.gotModel != "fake-embedding" || chunks.gotTopK != 5 {
		t.Errorf("search model/top_k = %q/%d, want fake-embedding/5", chunks.gotModel, chunks.gotTopK)
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
	if embedder.calls != 1 || chunks.calls != 1 {
		t.Errorf("embed/search called %d/%d times with nothing readable, want both once", embedder.calls, chunks.calls)
	}
	if len(chunks.gotDocumentIDs) != 0 {
		t.Errorf("search document IDs = %v, want the empty readable set", chunks.gotDocumentIDs)
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
	chunks := &fakeChunkSearch{err: searchErr}
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
		"unknown technique": {Query: "who won?", RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{"bm25"}}},
		"duplicate":         {Query: "who won?", RetrievalConfig: &types.RetrievalConfig{Techniques: []types.RetrievalTechnique{types.RetrievalTechniqueDense, types.RetrievalTechniqueDense}}},
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
			if docs.calls+embedder.calls+chunks.calls != 0 {
				t.Errorf("dependencies called %d/%d/%d times for an invalid request, want none",
					docs.calls, embedder.calls, chunks.calls)
			}
		})
	}
}

func TestRetrieveNeedsTheAuthenticatedPrincipal(t *testing.T) {
	retrieval := NewRetrievalService(&fakeReadableDocuments{}, &fakeChunkSearch{}, &fakeEmbedder{})

	_, err := retrieval.Retrieve(context.Background(), &types.RetrievalRequest{Query: "who won?"})
	if err == nil || !strings.Contains(err.Error(), "principal") {
		t.Errorf("Retrieve without a Principal = %v, want an error naming the missing Principal", err)
	}
}
