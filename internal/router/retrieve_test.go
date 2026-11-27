package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/service"
	"github.com/phongnd2802/trelis/internal/config"
	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/handler"
	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/models/embedding"
	"github.com/phongnd2802/trelis/internal/models/modeltest"
	"github.com/phongnd2802/trelis/internal/models/rerank"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// stubRetrieval answers with a fixed result, so the router tests can check
// the HTTP shape without an embedding model or a database.
type stubRetrieval struct {
	result *types.RetrievalResult
	err    error
	got    *types.RetrievalRequest
}

func (s *stubRetrieval) Retrieve(_ context.Context, req *types.RetrievalRequest) (*types.RetrievalResult, error) {
	s.got = req
	return s.result, s.err
}

// callerReadableDocuments answers the readable set per Principal, so a test
// can give two callers different access.
type callerReadableDocuments struct {
	interfaces.DocumentRepository
	readable map[uuid.UUID][]uuid.UUID
}

func (f *callerReadableDocuments) ListReadableDocumentIDs(_ context.Context, principalID uuid.UUID) ([]uuid.UUID, error) {
	return f.readable[principalID], nil
}

// readableChunkSearch answers only the Chunks of the Documents it was given,
// the way the SQL does.
type readableChunkSearch struct {
	interfaces.ChunkSearcher
	matches []*types.ChunkMatch
}

func (f *readableChunkSearch) SearchDense(_ context.Context, documentIDs []uuid.UUID,
	_ []float32, _ string, _ int,
) ([]*types.ChunkMatch, error) {
	readable := make(map[uuid.UUID]bool, len(documentIDs))
	for _, id := range documentIDs {
		readable[id] = true
	}
	var out []*types.ChunkMatch
	for _, match := range f.matches {
		if readable[match.Chunk.DocumentID] {
			out = append(out, match)
		}
	}
	return out, nil
}

// fixedEmbedder answers one fixed vector for any text.
type fixedEmbedder struct{ model string }

func (f *fixedEmbedder) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return [][]float32{{1, 0}}, nil
}

func (f *fixedEmbedder) GetModelName() string { return f.model }

// newRetrieveRouter is the router on one RetrievalService, with the Document
// routes unmounted because these tests never call them.
func newRetrieveRouter(retrieval interfaces.RetrievalService) http.Handler {
	me := handler.NewPrincipalHandler(service.NewPrincipalService(nil, nil, nil))
	return New(Readiness{}, keyring{"trelis_alice": alice}, me, nil,
		handler.NewRetrievalHandler(retrieval), nil, nil, false)
}

// postJSON sends body as the named bearer token (none when empty).
func postJSON(t *testing.T, h http.Handler, path, body, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// intPointer returns a pointer to n, for an explicitly reported candidate
// count.
func intPointer(n int) *int { return &n }

func TestRetrieveNeedsAKey(t *testing.T) {
	h := newRetrieveRouter(&stubRetrieval{})
	for _, authorization := range []string{"", "Bearer wrong"} {
		rec := postJSON(t, h, "/retrieve", `{"query":"who won?"}`, authorization)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST /retrieve with %q = %d, want 401", authorization, rec.Code)
		}
	}
}

func TestABodyThatIsNotOneJSONObjectIsBadRequest(t *testing.T) {
	h := newRetrieveRouter(&stubRetrieval{})
	for name, body := range map[string]string{
		"empty":         "",
		"not json":      "not json",
		"truncated":     `{"query":`,
		"array":         `[]`,
		"null":          "null",
		"trailing json": `{"query":"who won?"}{"query":"again"}`,
	} {
		rec := postJSON(t, h, "/retrieve", body, "Bearer trelis_alice")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: POST /retrieve = %d %s, want 400", name, rec.Code, rec.Body)
			continue
		}
		if e := decode(t, rec); e.Success || e.Error == nil || e.Error.Code != 1000 {
			t.Errorf("%s: body = %s, want the bad-request error envelope", name, rec.Body)
		}
	}
}

func TestAnUnknownFieldOrWrongTypeIsValidation(t *testing.T) {
	h := newRetrieveRouter(&stubRetrieval{})
	for name, body := range map[string]string{
		"unknown field": `{"query":"who won?","retrival_config":{"techniques":["dense"]}}`,
		"wrong top_k":   `{"query":"who won?","top_k":"ten"}`,
		"wrong query":   `{"query":42}`,
	} {
		rec := postJSON(t, h, "/retrieve", body, "Bearer trelis_alice")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: POST /retrieve = %d %s, want 400", name, rec.Code, rec.Body)
			continue
		}
		if e := decode(t, rec); e.Success || e.Error == nil || e.Error.Code != 1010 {
			t.Errorf("%s: body = %s, want the validation error envelope", name, rec.Body)
		}
	}
}

func TestAnInvalidRequestIsValidation(t *testing.T) {
	// Validation runs before any repository or model call, so the service
	// needs neither here.
	h := newRetrieveRouter(service.NewRetrievalService(nil, nil, nil, nil))
	for name, tc := range map[string]struct {
		body    string
		wantMsg []string
	}{
		"foo": {
			body:    `{"query":"who won?","retrieval_config":{"techniques":["foo"]}}`,
			wantMsg: []string{"foo", "dense"},
		},
		"empty techniques": {
			body:    `{"query":"who won?","retrieval_config":{"techniques":[]}}`,
			wantMsg: []string{"at least one", "dense"},
		},
		"duplicate": {
			body:    `{"query":"who won?","retrieval_config":{"techniques":["dense","dense"]}}`,
			wantMsg: []string{"dense", "more than once"},
		},
		"rrf_k zero": {
			body:    `{"query":"who won?","retrieval_config":{"techniques":["bm25","dense"],"rrf_k":0}}`,
			wantMsg: []string{"rrf_k", "1 and 1000"},
		},
		"rrf_k too large": {
			body:    `{"query":"who won?","retrieval_config":{"techniques":["dense"],"rrf_k":1001}}`,
			wantMsg: []string{"rrf_k", "1 and 1000"},
		},
		"rerank alone": {
			body:    `{"query":"who won?","retrieval_config":{"techniques":["rerank"]}}`,
			wantMsg: []string{"rerank", "at least one"},
		},
		"rerank_candidates below top_k": {
			body:    `{"query":"who won?","top_k":10,"retrieval_config":{"techniques":["bm25","rerank"],"rerank_candidates":5}}`,
			wantMsg: []string{"rerank_candidates", "top_k"},
		},
		"rerank_candidates too large": {
			body:    `{"query":"who won?","retrieval_config":{"techniques":["bm25","rerank"],"rerank_candidates":101}}`,
			wantMsg: []string{"rerank_candidates", "1 and 100"},
		},
		"blank query": {
			body:    `{"query":"   "}`,
			wantMsg: []string{"query"},
		},
		"top_k zero": {
			body:    `{"query":"who won?","top_k":0}`,
			wantMsg: []string{"top_k"},
		},
		"top_k too large": {
			body:    `{"query":"who won?","top_k":101}`,
			wantMsg: []string{"top_k"},
		},
		"query too long": {
			body:    `{"query":"` + strings.Repeat("a", types.RetrievalQueryMaxRunes+1) + `"}`,
			wantMsg: []string{"2000"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := postJSON(t, h, "/retrieve", tc.body, "Bearer trelis_alice")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST /retrieve = %d %s, want 400", rec.Code, rec.Body)
			}
			e := decode(t, rec)
			if e.Success || e.Error == nil || e.Error.Code != 1010 {
				t.Fatalf("body = %s, want the validation error envelope", rec.Body)
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(e.Error.Message, want) {
					t.Errorf("message %q does not name %q", e.Error.Message, want)
				}
			}
		})
	}
}

func TestARetrieveBodyOverTheLimitIs413(t *testing.T) {
	h := newRetrieveRouter(&stubRetrieval{})
	body := `{"query":"` + strings.Repeat("a", 70<<10) + `"}`

	rec := postJSON(t, h, "/retrieve", body, "Bearer trelis_alice")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST /retrieve with a %d-byte body = %d %s, want 413", len(body), rec.Code, rec.Body)
	}
	e := decode(t, rec)
	if e.Success || e.Error == nil || e.Error.Code != 1011 || !strings.Contains(e.Error.Message, "64KB") {
		t.Errorf("body = %s, want a 1011 error naming the 64KB limit", rec.Body)
	}
}

func TestRetrieveSendsTheBM25ConfigToTheService(t *testing.T) {
	svc := &stubRetrieval{result: &types.RetrievalResult{
		Results: []types.RetrievedChunk{},
		Trace:   []types.TraceStep{{Step: types.TraceStepSearch, Technique: types.RetrievalTechniqueBM25}},
	}}
	h := newRetrieveRouter(svc)

	rec := postJSON(t, h, "/retrieve",
		`{"query":"thue tncn","retrieval_config":{"techniques":["bm25"]}}`, "Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /retrieve = %d %s, want 200", rec.Code, rec.Body)
	}
	if svc.got == nil || svc.got.Query != "thue tncn" || svc.got.RetrievalConfig == nil ||
		len(svc.got.RetrievalConfig.Techniques) != 1 ||
		svc.got.RetrievalConfig.Techniques[0] != types.RetrievalTechniqueBM25 {
		t.Errorf("service got %+v, want the bm25 request", svc.got)
	}
	if !strings.Contains(rec.Body.String(), `"technique":"bm25"`) {
		t.Errorf("body = %s, want the bm25 search step", rec.Body)
	}
}

func TestRetrieveSendsSeveralTechniquesAndRRFKToTheService(t *testing.T) {
	svc := &stubRetrieval{result: &types.RetrievalResult{
		Results: []types.RetrievedChunk{},
		Trace:   []types.TraceStep{},
	}}
	h := newRetrieveRouter(svc)

	rec := postJSON(t, h, "/retrieve",
		`{"query":"thue tncn","retrieval_config":{"techniques":["bm25","dense"],"rrf_k":10}}`, "Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /retrieve = %d %s, want 200", rec.Code, rec.Body)
	}
	config := svc.got.RetrievalConfig
	if config == nil || len(config.Techniques) != 2 ||
		config.Techniques[0] != types.RetrievalTechniqueBM25 ||
		config.Techniques[1] != types.RetrievalTechniqueDense {
		t.Fatalf("service got %+v, want bm25 and dense", config)
	}
	if config.RRFK == nil || *config.RRFK != 10 {
		t.Errorf("service got rrf_k = %v, want 10", config.RRFK)
	}
}

func TestRetrieveAnswersTheResultEnvelope(t *testing.T) {
	chunkID := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	documentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	svc := &stubRetrieval{result: &types.RetrievalResult{
		Results: []types.RetrievedChunk{{
			ChunkID: chunkID, DocumentID: documentID, Seq: 3, Content: "Điều 1.",
			ContextHeader: "Chương I", Score: 0.83,
			Scores: map[types.RetrievalTechnique]float64{types.RetrievalTechniqueDense: 0.83},
			Ranks:  map[types.RetrievalTechnique]int{types.RetrievalTechniqueDense: 1},
		}},
		Trace: []types.TraceStep{
			{Step: types.TraceStepEmbedQuery, Technique: types.RetrievalTechniqueDense, DurationMS: 12, Model: "test-embedding"},
			{Step: types.TraceStepSearch, Technique: types.RetrievalTechniqueDense, DurationMS: 4, Candidates: intPointer(1)},
		},
	}}
	h := newRetrieveRouter(svc)

	rec := postJSON(t, h, "/retrieve",
		`{"query":"who won?","top_k":3,"retrieval_config":{"techniques":["dense"]}}`,
		"Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /retrieve = %d %s, want 200", rec.Code, rec.Body)
	}
	e := decode(t, rec)
	if !e.Success {
		t.Fatalf("success = false in %s", rec.Body)
	}
	var got types.RetrievalResult
	if err := json.Unmarshal(e.Data, &got); err != nil {
		t.Fatalf("data in %s is not a RetrievalResult: %v", rec.Body, err)
	}
	if len(got.Results) != 1 {
		t.Fatalf("results = %+v, want one", got.Results)
	}
	r := got.Results[0]
	if r.ChunkID != chunkID || r.DocumentID != documentID || r.Seq != 3 || r.Content != "Điều 1." ||
		r.ContextHeader != "Chương I" || r.Score != 0.83 ||
		r.Scores[types.RetrievalTechniqueDense] != 0.83 ||
		r.Ranks[types.RetrievalTechniqueDense] != 1 {
		t.Errorf("result = %+v, want the stub's Chunk, scores and ranks", r)
	}
	if len(got.Trace) != 2 || got.Trace[0].Step != types.TraceStepEmbedQuery || got.Trace[0].Model != "test-embedding" ||
		got.Trace[1].Candidates == nil || *got.Trace[1].Candidates != 1 {
		t.Errorf("trace = %+v, want the embed and search steps", got.Trace)
	}
	if !strings.Contains(rec.Body.String(), `"ranks":{"dense":1}`) {
		t.Errorf("body = %s, want the per-technique ranks in the result", rec.Body)
	}
	if svc.got == nil || svc.got.Query != "who won?" || svc.got.TopK == nil || *svc.got.TopK != 3 ||
		svc.got.RetrievalConfig == nil || len(svc.got.RetrievalConfig.Techniques) != 1 ||
		svc.got.RetrievalConfig.Techniques[0] != types.RetrievalTechniqueDense {
		t.Errorf("service got %+v, want the decoded body", svc.got)
	}
}

func TestTheSearchStepAlwaysReportsItsCandidateCount(t *testing.T) {
	svc := &stubRetrieval{result: &types.RetrievalResult{
		Results: []types.RetrievedChunk{},
		Trace: []types.TraceStep{
			{Step: types.TraceStepEmbedQuery, Technique: types.RetrievalTechniqueDense, DurationMS: 1, Model: "test-embedding"},
			{Step: types.TraceStepSearch, Technique: types.RetrievalTechniqueDense, DurationMS: 1, Candidates: intPointer(0)},
		},
	}}
	h := newRetrieveRouter(svc)

	rec := postJSON(t, h, "/retrieve", `{"query":"who won?"}`, "Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /retrieve = %d %s, want 200", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"candidates":0`) {
		t.Errorf("body = %s, want the search step's candidates 0", rec.Body)
	}
	if strings.Count(rec.Body.String(), `"candidates"`) != 1 {
		t.Errorf("body = %s, want candidates only on the search step", rec.Body)
	}
}

func TestRetrieveSendsRerankCandidatesToTheService(t *testing.T) {
	svc := &stubRetrieval{result: &types.RetrievalResult{
		Results: []types.RetrievedChunk{},
		Trace:   []types.TraceStep{},
	}}
	h := newRetrieveRouter(svc)

	rec := postJSON(t, h, "/retrieve",
		`{"query":"thue tncn","retrieval_config":{"techniques":["bm25","rerank"],"rerank_candidates":7}}`,
		"Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /retrieve = %d %s, want 200", rec.Code, rec.Body)
	}
	config := svc.got.RetrievalConfig
	if config == nil || len(config.Techniques) != 2 ||
		config.Techniques[0] != types.RetrievalTechniqueBM25 ||
		config.Techniques[1] != types.RetrievalTechniqueRerank {
		t.Fatalf("service got %+v, want bm25 and rerank", config)
	}
	if config.RerankCandidates == nil || *config.RerankCandidates != 7 {
		t.Errorf("service got rerank_candidates = %v, want 7", config.RerankCandidates)
	}
}

func TestRetrieveWithoutARerankModelIs503(t *testing.T) {
	// Dense is available, so the request reaches the rerank check; the rerank
	// role is not configured.
	h := newRetrieveRouter(service.NewRetrievalService(nil, nil, &fixedEmbedder{model: "test-embedding"}, nil))

	rec := postJSON(t, h, "/retrieve",
		`{"query":"who won?","retrieval_config":{"techniques":["dense","rerank"]}}`, "Bearer trelis_alice")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /retrieve = %d %s, want 503", rec.Code, rec.Body)
	}
	e := decode(t, rec)
	if e.Success || e.Error == nil || e.Error.Code != 1008 || !strings.Contains(e.Error.Message, "models.rerank") {
		t.Errorf("body = %s, want a 1008 error naming models.rerank", rec.Body)
	}
}

func TestRetrieveWithoutAnEmbeddingModelIs503(t *testing.T) {
	h := newRetrieveRouter(service.NewRetrievalService(nil, nil, nil, nil))

	rec := postJSON(t, h, "/retrieve", `{"query":"who won?"}`, "Bearer trelis_alice")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /retrieve = %d %s, want 503", rec.Code, rec.Body)
	}
	e := decode(t, rec)
	if e.Success || e.Error == nil || e.Error.Code != 1008 || !strings.Contains(e.Error.Message, "models.embedding") {
		t.Errorf("body = %s, want a 1008 error naming models.embedding", rec.Body)
	}
}

func TestAnInternalRetrievalFailureIs500(t *testing.T) {
	h := newRetrieveRouter(&stubRetrieval{err: errors.New("postgres down")})

	rec := postJSON(t, h, "/retrieve", `{"query":"who won?"}`, "Bearer trelis_alice")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /retrieve = %d %s, want 500", rec.Code, rec.Body)
	}
	e := decode(t, rec)
	if e.Success || e.Error == nil || e.Error.Code != 1007 || e.Error.Message != "Internal server error" {
		t.Errorf("body = %s, want the generic internal error envelope", rec.Body)
	}
}

// newProviderRetrieveRouter wires the real embedding and rerank clients to p,
// so a test can see how a provider's answer reaches the client through
// POST /retrieve.
func newProviderRetrieveRouter(t *testing.T, p *modeltest.FakeProvider) http.Handler {
	t.Helper()
	embedder, err := embedding.New(
		config.EmbeddingRole{
			Endpoint:  config.Endpoint{BaseURL: p.URL(), Model: "test-embedding"},
			BatchSize: 8,
		},
		embedding.WithRetry(1, 0, 0),
	)
	if err != nil {
		t.Fatal(err)
	}
	reranker, err := rerank.New(
		config.RerankRole{Endpoint: config.Endpoint{BaseURL: p.URL(), Model: "test-rerank"}},
		rerank.WithRetry(api.Retry{MaxAttempts: 1}),
	)
	if err != nil {
		t.Fatal(err)
	}
	documentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	chunkID := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	svc := service.NewRetrievalService(
		&callerReadableDocuments{readable: map[uuid.UUID][]uuid.UUID{alice.ID: {documentID}}},
		&readableChunkSearch{matches: []*types.ChunkMatch{{
			Chunk: &types.Chunk{ID: chunkID, DocumentID: documentID, Content: "Alice's readable Chunk"},
			Score: 1,
		}}},
		embedder, reranker,
	)
	return newRetrieveRouter(svc)
}

// assertModelProviderRejection checks the 502 envelope and that the provider's
// own body is nowhere in it.
func assertModelProviderRejection(t *testing.T, rec *httptest.ResponseRecorder, wantMessage string) {
	t.Helper()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("POST /retrieve = %d %s, want 502", rec.Code, rec.Body)
	}
	e := decode(t, rec)
	if e.Success || e.Error == nil {
		t.Fatalf("body = %s, want the error envelope", rec.Body)
	}
	if e.Error.Code != int(apperrors.ErrModelProvider) {
		t.Errorf("code = %d, want %d", e.Error.Code, apperrors.ErrModelProvider)
	}
	if e.Error.Message != wantMessage {
		t.Errorf("message = %q, want %q", e.Error.Message, wantMessage)
	}
	if strings.Contains(rec.Body.String(), "modeltest") {
		t.Errorf("body = %s, want no provider error body", rec.Body)
	}
}

func TestARerankProviderRejectionIs502WithoutTheVendorBody(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.FailNext(modeltest.Rerank, http.StatusPaymentRequired, 1)
	h := newProviderRetrieveRouter(t, p)

	rec := postJSON(t, h, "/retrieve",
		`{"query":"who won?","retrieval_config":{"techniques":["dense","rerank"]}}`,
		"Bearer trelis_alice")

	assertModelProviderRejection(t, rec, "the rerank model provider rejected the call (HTTP 402)")
}

func TestAnEmbeddingProviderRejectionIs502WithoutTheVendorBody(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.FailNext(modeltest.Embeddings, http.StatusUnauthorized, 1)
	h := newProviderRetrieveRouter(t, p)

	rec := postJSON(t, h, "/retrieve", `{"query":"who won?"}`, "Bearer trelis_alice")

	assertModelProviderRejection(t, rec, "the embedding model provider rejected the call (HTTP 401)")
}

func TestAnotherProviderFailureIsStillAGeneric500(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.FailNext(modeltest.Rerank, http.StatusInternalServerError, 1)
	h := newProviderRetrieveRouter(t, p)

	rec := postJSON(t, h, "/retrieve",
		`{"query":"who won?","retrieval_config":{"techniques":["dense","rerank"]}}`,
		"Bearer trelis_alice")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /retrieve = %d %s, want 500", rec.Code, rec.Body)
	}
	e := decode(t, rec)
	if e.Success || e.Error == nil || e.Error.Code != int(apperrors.ErrInternalServer) ||
		e.Error.Message != "Internal server error" {
		t.Errorf("body = %s, want the generic internal error envelope", rec.Body)
	}
}

// TestAnotherPrincipalNeverReceivesAChunkAtTheRouterSeam gives two callers
// different readable sets and asserts the answer follows the caller, the
// access case CODING_STANDARDS requires for every new read path.
func TestAnotherPrincipalNeverReceivesAChunkAtTheRouterSeam(t *testing.T) {
	bob := types.Principal{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000002"), Name: "bob"}
	aliceDoc := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	bobDoc := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b2")
	aliceChunk := &types.ChunkMatch{
		Chunk: &types.Chunk{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000001"), DocumentID: aliceDoc, Content: "alice secret"},
		Score: 1,
	}
	bobChunk := &types.ChunkMatch{
		Chunk: &types.Chunk{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000003"), DocumentID: bobDoc, Content: "bob note"},
		Score: 0.5,
	}
	svc := service.NewRetrievalService(
		&callerReadableDocuments{readable: map[uuid.UUID][]uuid.UUID{
			alice.ID: {aliceDoc},
			bob.ID:   {bobDoc},
		}},
		&readableChunkSearch{matches: []*types.ChunkMatch{aliceChunk, bobChunk}},
		&fixedEmbedder{model: "test-embedding"}, nil,
	)
	me := handler.NewPrincipalHandler(service.NewPrincipalService(nil, nil, nil))
	h := New(Readiness{}, keyring{"trelis_alice": alice, "trelis_bob": bob}, me, nil,
		handler.NewRetrievalHandler(svc), nil, nil, false)

	bobRec := postJSON(t, h, "/retrieve", `{"query":"who won?"}`, "Bearer trelis_bob")
	if bobRec.Code != http.StatusOK {
		t.Fatalf("bob's POST /retrieve = %d %s, want 200", bobRec.Code, bobRec.Body)
	}
	var bobGot types.RetrievalResult
	if err := json.Unmarshal(decode(t, bobRec).Data, &bobGot); err != nil {
		t.Fatal(err)
	}
	if len(bobGot.Results) != 1 || bobGot.Results[0].DocumentID != bobDoc {
		t.Errorf("bob's results = %+v, want only his own Document %s", bobGot.Results, bobDoc)
	}
	for _, result := range bobGot.Results {
		if result.DocumentID == aliceDoc {
			t.Errorf("bob received Chunk %s of alice's Document", result.ChunkID)
		}
	}

	aliceRec := postJSON(t, h, "/retrieve", `{"query":"who won?"}`, "Bearer trelis_alice")
	if aliceRec.Code != http.StatusOK {
		t.Fatalf("alice's POST /retrieve = %d %s, want 200", aliceRec.Code, aliceRec.Body)
	}
	var aliceGot types.RetrievalResult
	if err := json.Unmarshal(decode(t, aliceRec).Data, &aliceGot); err != nil {
		t.Fatal(err)
	}
	if len(aliceGot.Results) != 1 || aliceGot.Results[0].DocumentID != aliceDoc {
		t.Errorf("alice's results = %+v, want only her own Document %s", aliceGot.Results, aliceDoc)
	}
}
