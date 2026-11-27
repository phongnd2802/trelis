package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/service"
	"github.com/phongnd2802/trelis/internal/config"
	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/handler"
	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/models/chat"
	"github.com/phongnd2802/trelis/internal/models/modeltest"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// stubAnswer answers with a fixed result and records the request, so the
// router tests can check the HTTP shape without a chat model.
type stubAnswer struct {
	result *types.AnswerResult
	err    error
	calls  int
	got    *types.AnswerRequest
}

func (s *stubAnswer) Answer(_ context.Context, req *types.AnswerRequest) (*types.AnswerResult, error) {
	s.calls++
	s.got = req
	return s.result, s.err
}

// newAnswerRouter is the router on one AnswerService, with the other routes
// unmounted because these tests never call them.
func newAnswerRouter(answer interfaces.AnswerService) http.Handler {
	me := handler.NewPrincipalHandler(service.NewPrincipalService(nil, nil, nil))
	return New(Readiness{}, keyring{"trelis_alice": alice}, me, nil, nil,
		handler.NewAnswerHandler(answer), nil, false)
}

func TestAnswerNeedsAKey(t *testing.T) {
	h := newAnswerRouter(&stubAnswer{})
	for _, authorization := range []string{"", "Bearer wrong"} {
		rec := postJSON(t, h, "/answer", `{"query":"who won?"}`, authorization)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST /answer with %q = %d, want 401", authorization, rec.Code)
		}
	}
}

func TestAnAnswerBodyThatIsNotOneJSONObjectIsBadRequest(t *testing.T) {
	h := newAnswerRouter(&stubAnswer{})
	for name, body := range map[string]string{
		"empty":         "",
		"not json":      "not json",
		"truncated":     `{"query":`,
		"array":         `[]`,
		"null":          "null",
		"trailing json": `{"query":"who won?"}{"query":"again"}`,
	} {
		rec := postJSON(t, h, "/answer", body, "Bearer trelis_alice")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: POST /answer = %d %s, want 400", name, rec.Code, rec.Body)
			continue
		}
		if e := decode(t, rec); e.Success || e.Error == nil || e.Error.Code != 1000 {
			t.Errorf("%s: body = %s, want the bad-request error envelope", name, rec.Body)
		}
	}
}

func TestAnAnswerUnknownFieldOrWrongTypeIsValidation(t *testing.T) {
	h := newAnswerRouter(&stubAnswer{})
	for name, body := range map[string]string{
		"unknown field": `{"query":"who won?","debug_mode":true}`,
		"wrong top_k":   `{"query":"who won?","top_k":"ten"}`,
		"wrong debug":   `{"query":"who won?","debug":"yes"}`,
	} {
		rec := postJSON(t, h, "/answer", body, "Bearer trelis_alice")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: POST /answer = %d %s, want 400", name, rec.Code, rec.Body)
			continue
		}
		if e := decode(t, rec); e.Success || e.Error == nil || e.Error.Code != 1010 {
			t.Errorf("%s: body = %s, want the validation error envelope", name, rec.Body)
		}
	}
}

func TestAnInvalidAnswerRequestIsValidation(t *testing.T) {
	// Validation runs before the chat-role check, so no service is needed
	// behind it.
	h := newAnswerRouter(service.NewAnswerService(nil, nil))
	for name, body := range map[string]string{
		"blank query":       `{"query":"   "}`,
		"top_k zero":        `{"query":"who won?","top_k":0}`,
		"unknown technique": `{"query":"who won?","retrieval_config":{"techniques":["foo"]}}`,
		"rerank alone":      `{"query":"who won?","retrieval_config":{"techniques":["rerank"]}}`,
	} {
		rec := postJSON(t, h, "/answer", body, "Bearer trelis_alice")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: POST /answer = %d %s, want 400", name, rec.Code, rec.Body)
			continue
		}
		if e := decode(t, rec); e.Success || e.Error == nil || e.Error.Code != 1010 {
			t.Errorf("%s: body = %s, want the validation error envelope", name, rec.Body)
		}
	}
}

func TestAnAnswerBodyOverTheLimitIs413(t *testing.T) {
	h := newAnswerRouter(&stubAnswer{})
	body := `{"query":"` + strings.Repeat("a", 70<<10) + `"}`

	rec := postJSON(t, h, "/answer", body, "Bearer trelis_alice")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST /answer with a %d-byte body = %d %s, want 413", len(body), rec.Code, rec.Body)
	}
	e := decode(t, rec)
	if e.Success || e.Error == nil || e.Error.Code != 1011 || !strings.Contains(e.Error.Message, "64KB") {
		t.Errorf("body = %s, want a 1011 error naming the 64KB limit", rec.Body)
	}
}

func TestAnswerWithoutAChatModelIs503(t *testing.T) {
	h := newAnswerRouter(service.NewAnswerService(nil, nil))

	rec := postJSON(t, h, "/answer", `{"query":"who won?"}`, "Bearer trelis_alice")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /answer = %d %s, want 503", rec.Code, rec.Body)
	}
	e := decode(t, rec)
	if e.Success || e.Error == nil || e.Error.Code != 1008 || !strings.Contains(e.Error.Message, "models.chat") {
		t.Errorf("body = %s, want a 1008 error naming models.chat", rec.Body)
	}
}

func TestAnswerSendsTheRequestToTheService(t *testing.T) {
	svc := &stubAnswer{result: &types.AnswerResult{Answer: "yes", Citations: []types.Citation{}}}
	h := newAnswerRouter(svc)

	rec := postJSON(t, h, "/answer",
		`{"query":"thue tncn","top_k":4,"retrieval_config":{"techniques":["bm25"],"rrf_k":10},"debug":true}`,
		"Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /answer = %d %s, want 200", rec.Code, rec.Body)
	}
	got := svc.got
	if got == nil || got.Query != "thue tncn" || got.TopK == nil || *got.TopK != 4 || !got.Debug {
		t.Fatalf("service got %+v, want the decoded answer request with debug", got)
	}
	if got.RetrievalConfig == nil || len(got.RetrievalConfig.Techniques) != 1 ||
		got.RetrievalConfig.Techniques[0] != types.RetrievalTechniqueBM25 ||
		got.RetrievalConfig.RRFK == nil || *got.RetrievalConfig.RRFK != 10 {
		t.Errorf("service got retrieval_config %+v, want bm25 with rrf_k 10", got.RetrievalConfig)
	}
}

func TestAnswerAnswersTheResultEnvelopeWithAndWithoutDebug(t *testing.T) {
	chunkID := uuid.MustParse("0190d2a8-0000-7000-8000-000000000001")
	documentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1")
	results := []types.RetrievedChunk{{
		ChunkID: chunkID, DocumentID: documentID, Seq: 3, Content: "Điều 1.",
		ContextHeader: "Chương I", Score: 0.83,
		Scores: map[types.RetrievalTechnique]float64{types.RetrievalTechniqueBM25: 0.83},
		Ranks:  map[types.RetrievalTechnique]int{types.RetrievalTechniqueBM25: 1},
	}}
	svc := &stubAnswer{result: &types.AnswerResult{
		Answer:       "Điều 1 [1].",
		Citations:    []types.Citation{{Marker: 1, ChunkID: chunkID, DocumentID: documentID}},
		FinishReason: "stop",
		Debug: &types.AnswerDebug{
			Results: results,
			Trace: []types.TraceStep{
				{Step: types.TraceStepSearch, Technique: types.RetrievalTechniqueBM25, DurationMS: 4, Candidates: intPointer(1)},
				{Step: types.TraceStepGenerate, DurationMS: 9, Model: "test-chat"},
			},
			Usage: types.TokenUsage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
		},
	}}
	h := newAnswerRouter(svc)

	rec := postJSON(t, h, "/answer", `{"query":"who won?","debug":true}`, "Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /answer = %d %s, want 200", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"debug"`) {
		t.Errorf("body = %s, want the debug object when debug is true", rec.Body)
	}
	var got types.AnswerResult
	if err := json.Unmarshal(decode(t, rec).Data, &got); err != nil {
		t.Fatalf("data in %s is not an AnswerResult: %v", rec.Body, err)
	}
	if got.Answer != "Điều 1 [1]." || len(got.Citations) != 1 ||
		got.Citations[0] != (types.Citation{Marker: 1, ChunkID: chunkID, DocumentID: documentID}) ||
		got.FinishReason != "stop" {
		t.Errorf("result = %+v, want the stub's answer, citation and finish reason", got)
	}
	if got.Debug == nil || len(got.Debug.Results) != 1 || len(got.Debug.Trace) != 2 ||
		got.Debug.Usage.TotalTokens != 18 {
		t.Errorf("debug = %+v, want the stub's results, trace and usage", got.Debug)
	}
	if !strings.Contains(rec.Body.String(), `"step":"generate"`) {
		t.Errorf("body = %s, want the generate step", rec.Body)
	}

	// Without debug the response has no debug key at all.
	plain := &stubAnswer{result: &types.AnswerResult{Answer: "yes", Citations: []types.Citation{}}}
	plainRec := postJSON(t, newAnswerRouter(plain), "/answer", `{"query":"who won?"}`, "Bearer trelis_alice")
	if plainRec.Code != http.StatusOK {
		t.Fatalf("POST /answer without debug = %d %s, want 200", plainRec.Code, plainRec.Body)
	}
	if strings.Contains(plainRec.Body.String(), `"debug"`) {
		t.Errorf("body = %s, want no debug key without debug", plainRec.Body)
	}
	if !strings.Contains(plainRec.Body.String(), `"citations":[]`) {
		t.Errorf("body = %s, want citations as [] rather than null", plainRec.Body)
	}
}

func TestAnInternalAnswerFailureIs500(t *testing.T) {
	h := newAnswerRouter(&stubAnswer{err: errors.New("chat provider down")})

	rec := postJSON(t, h, "/answer", `{"query":"who won?"}`, "Bearer trelis_alice")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /answer = %d %s, want 500", rec.Code, rec.Body)
	}
	e := decode(t, rec)
	if e.Success || e.Error == nil || e.Error.Code != 1007 || e.Error.Message != "Internal server error" {
		t.Errorf("body = %s, want the generic internal error envelope", rec.Body)
	}
}

func TestAChatProviderRejectionIs502WithoutTheVendorBody(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.FailNext(modeltest.ChatCompletions, http.StatusUnauthorized, 1)
	c, err := chat.NewChat("chat", config.ChatRole{
		Protocol: api.APIOpenAICompletions,
		Endpoint: config.Endpoint{BaseURL: p.URL(), Model: "test-chat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	retrieval := &stubRetrieval{result: &types.RetrievalResult{
		Results: []types.RetrievedChunk{{Content: "The election was in 2024."}},
		Trace:   []types.TraceStep{},
	}}
	h := newAnswerRouter(service.NewAnswerService(retrieval, chat.Generator(c)))

	rec := postJSON(t, h, "/answer", `{"query":"who won?"}`, "Bearer trelis_alice")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("POST /answer = %d %s, want 502", rec.Code, rec.Body)
	}
	e := decode(t, rec)
	if e.Success || e.Error == nil {
		t.Fatalf("body = %s, want the error envelope", rec.Body)
	}
	if e.Error.Code != int(apperrors.ErrModelProvider) {
		t.Errorf("code = %d, want %d", e.Error.Code, apperrors.ErrModelProvider)
	}
	if want := "the chat model provider rejected the call (HTTP 401)"; e.Error.Message != want {
		t.Errorf("message = %q, want %q", e.Error.Message, want)
	}
	if strings.Contains(rec.Body.String(), "modeltest") {
		t.Errorf("body = %s, want no provider error body", rec.Body)
	}
}
