package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// stubAnswerRetrieval records the retrieval request and answers a fixed
// result, so the answer tests can check what the service asked retrieval for
// without a database or an embedding model.
type stubAnswerRetrieval struct {
	result *types.RetrievalResult
	err    error
	calls  int
	got    *types.RetrievalRequest
}

func (s *stubAnswerRetrieval) Retrieve(_ context.Context, req *types.RetrievalRequest) (*types.RetrievalResult, error) {
	s.calls++
	s.got = req
	return s.result, s.err
}

// forbiddenAnswerRetrieval fails the test when the answer service retrieves.
type forbiddenAnswerRetrieval struct{ t *testing.T }

func (f *forbiddenAnswerRetrieval) Retrieve(context.Context, *types.RetrievalRequest) (*types.RetrievalResult, error) {
	f.t.Error("retrieval called when the answer must not retrieve")
	return nil, nil
}

// fakeGenerator records the turns it was sent and answers a fixed reply.
type fakeGenerator struct {
	reply    *types.ChatResponse
	err      error
	model    string
	calls    int
	gotTurns []types.ChatTurn
}

func (f *fakeGenerator) Generate(_ context.Context, turns []types.ChatTurn) (*types.ChatResponse, error) {
	f.calls++
	f.gotTurns = append([]types.ChatTurn(nil), turns...)
	return f.reply, f.err
}

func (f *fakeGenerator) GetModelName() string { return f.model }

// forbiddenAnswerGenerator fails the test when the answer service calls the
// chat role.
type forbiddenAnswerGenerator struct{ t *testing.T }

func (f *forbiddenAnswerGenerator) Generate(context.Context, []types.ChatTurn) (*types.ChatResponse, error) {
	f.t.Error("chat role called when the answer must not call it")
	return nil, nil
}

func (f *forbiddenAnswerGenerator) GetModelName() string { return "forbidden" }

// answerResults are three retrieved Chunks with distinct text, headers and
// IDs.
func answerResults() []types.RetrievedChunk {
	return []types.RetrievedChunk{
		{
			ChunkID:       uuid.MustParse("0190d2a8-0000-7000-8000-000000000001"),
			DocumentID:    uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1"),
			Seq:           0,
			Content:       "Điều 1. Phạm vi điều chỉnh.",
			ContextHeader: "Chương I",
		},
		{
			ChunkID:    uuid.MustParse("0190d2a8-0000-7000-8000-000000000002"),
			DocumentID: uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b2"),
			Seq:        1,
			Content:    "Điều 2. Đối tượng áp dụng.",
		},
		{
			ChunkID:       uuid.MustParse("0190d2a8-0000-7000-8000-000000000003"),
			DocumentID:    uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b3"),
			Seq:           2,
			Content:       "Điều 3. Giải thích từ ngữ.",
			ContextHeader: "Chương II",
		},
	}
}

// answerRequest is a valid answer request with explicit values, so a test can
// assert they reach retrieval unchanged.
func answerRequest(query string, debug bool) *types.AnswerRequest {
	topK, rrfK, candidates := 3, 10, 7
	return &types.AnswerRequest{
		RetrievalRequest: types.RetrievalRequest{
			Query: query,
			TopK:  &topK,
			RetrievalConfig: &types.RetrievalConfig{
				Techniques:       []types.RetrievalTechnique{types.RetrievalTechniqueBM25, types.RetrievalTechniqueDense},
				RRFK:             &rrfK,
				RerankCandidates: &candidates,
			},
		},
		Debug: debug,
	}
}

// answerWith runs one answer against results and a generator reply.
func answerWith(t *testing.T, results []types.RetrievedChunk, reply *types.ChatResponse, debug bool) *types.AnswerResult {
	t.Helper()
	svc := NewAnswerService(&stubAnswerRetrieval{result: &types.RetrievalResult{Results: results}}, &fakeGenerator{reply: reply})
	got, err := svc.Answer(callerContext(uuid.New()), answerRequest("who won?", debug))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	return got
}

func TestAnswerCitesEachRetrievedSourceOnceInFirstAppearanceOrder(t *testing.T) {
	results := answerResults()
	got := answerWith(t, results, &types.ChatResponse{
		Content:      "Alpha [1] and gamma [3]. Beta [1][3].",
		FinishReason: "stop",
	}, false)

	if want := "Alpha [1] and gamma [3]. Beta [1][3]."; got.Answer != want {
		t.Errorf("answer = %q, want %q with the valid markers kept", got.Answer, want)
	}
	want := []types.Citation{
		{Marker: 1, ChunkID: results[0].ChunkID, DocumentID: results[0].DocumentID},
		{Marker: 3, ChunkID: results[2].ChunkID, DocumentID: results[2].DocumentID},
	}
	if !reflect.DeepEqual(got.Citations, want) {
		t.Errorf("citations = %+v, want %+v", got.Citations, want)
	}
	if got.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", got.FinishReason)
	}
}

func TestAnswerParsesCommaSeparatedMarkers(t *testing.T) {
	got := answerWith(t, answerResults(), &types.ChatResponse{
		Content:      "Both [1, 3] and then [2,3].",
		FinishReason: "stop",
	}, false)

	wantMarkers := []int{1, 3, 2}
	gotMarkers := make([]int, len(got.Citations))
	for i, c := range got.Citations {
		gotMarkers[i] = c.Marker
	}
	if !reflect.DeepEqual(gotMarkers, wantMarkers) {
		t.Errorf("citation markers = %v, want %v in order of first appearance", gotMarkers, wantMarkers)
	}
	for _, want := range []string{"[1, 3]", "[2, 3]"} {
		if !strings.Contains(got.Answer, want) {
			t.Errorf("answer %q does not keep the marker %s", got.Answer, want)
		}
	}
}

func TestAnOutOfRangeMarkerIsDropped(t *testing.T) {
	got := answerWith(t, answerResults(), &types.ChatResponse{
		Content:      "Alpha [1], nowhere [9], mixed [1, 9].",
		FinishReason: "stop",
	}, false)

	if want := "Alpha [1], nowhere, mixed [1]."; got.Answer != want {
		t.Errorf("answer = %q, want %q with the citations outside 1..3 and their leading space removed", got.Answer, want)
	}
	if len(got.Citations) != 1 || got.Citations[0].Marker != 1 {
		t.Errorf("citations = %+v, want only source 1", got.Citations)
	}
}

func TestAMarkerInsideCodeIsNotACitation(t *testing.T) {
	content := "Inline `arr[9]` here [1].\n\n```go\nvalues[3]\n```\n\nDone [2]."
	got := answerWith(t, answerResults(), &types.ChatResponse{Content: content, FinishReason: "stop"}, false)

	wantMarkers := []int{1, 2}
	gotMarkers := make([]int, len(got.Citations))
	for i, c := range got.Citations {
		gotMarkers[i] = c.Marker
	}
	if !reflect.DeepEqual(gotMarkers, wantMarkers) {
		t.Errorf("citation markers = %v, want only the markers outside code %v", gotMarkers, wantMarkers)
	}
	if got.Answer != content {
		t.Errorf("answer = %q, want the code left untouched and the valid markers kept", got.Answer)
	}
	for _, code := range []string{"`arr[9]`", "values[3]"} {
		if !strings.Contains(got.Answer, code) {
			t.Errorf("answer %q lost the code %s", got.Answer, code)
		}
	}
}

func TestAMarkdownLinkIsNotACitation(t *testing.T) {
	content := "See [2](https://example.com/thue) and [1]."
	got := answerWith(t, answerResults(), &types.ChatResponse{Content: content, FinishReason: "stop"}, false)

	if got.Answer != content {
		t.Errorf("answer = %q, want the Markdown link left untouched", got.Answer)
	}
	if len(got.Citations) != 1 || got.Citations[0].Marker != 1 {
		t.Errorf("citations = %+v, want only source 1 after the link", got.Citations)
	}
}

func TestTheNoSourceAnswerFollowsTheQuestionsLanguage(t *testing.T) {
	for name, tc := range map[string]struct {
		query string
		want  string
	}{
		"English question":              {"who won the election?", noSourcesAnswer},
		"Vietnamese question":           {"Thuế TNCN là gì?", noSourcesAnswerVietnamese},
		"Vietnamese without diacritics": {"thue tncn la gi?", noSourcesAnswer},
	} {
		t.Run(name, func(t *testing.T) {
			svc := NewAnswerService(
				&stubAnswerRetrieval{result: &types.RetrievalResult{Results: []types.RetrievedChunk{}}},
				&forbiddenAnswerGenerator{t},
			)
			got, err := svc.Answer(callerContext(uuid.New()), &types.AnswerRequest{
				RetrievalRequest: types.RetrievalRequest{Query: tc.query},
			})
			if err != nil {
				t.Fatalf("Answer: %v", err)
			}
			if got.Answer != tc.want {
				t.Errorf("answer = %q, want %q for the query %q", got.Answer, tc.want, tc.query)
			}
		})
	}
}

func TestTheChatRoleReceivesTheSourcesInRetrievalOrder(t *testing.T) {
	results := answerResults()
	gen := &fakeGenerator{reply: &types.ChatResponse{Content: "Answer [1].", FinishReason: "stop"}}
	retrieval := &stubAnswerRetrieval{result: &types.RetrievalResult{Results: results}}
	svc := NewAnswerService(retrieval, gen)

	if _, err := svc.Answer(callerContext(uuid.New()), answerRequest("who won?", false)); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if gen.calls != 1 {
		t.Fatalf("chat role called %d times, want once", gen.calls)
	}
	if len(gen.gotTurns) != 2 {
		t.Fatalf("turns = %+v, want a system and a user turn", gen.gotTurns)
	}
	if system := gen.gotTurns[0]; system.Role != types.ChatRoleSystem || system.Content == "" {
		t.Errorf("turn 0 = %+v, want a non-blank system prompt", system)
	}
	user := gen.gotTurns[1]
	if user.Role != types.ChatRoleUser {
		t.Errorf("turn 1 role = %q, want %q", user.Role, types.ChatRoleUser)
	}
	last := -1
	for i, result := range results {
		marker := fmt.Sprintf("[%d]", i+1)
		markerAt := strings.Index(user.Content, marker)
		text := chunkText(&types.Chunk{Content: result.Content, ContextHeader: result.ContextHeader})
		textAt := strings.Index(user.Content, text)
		if markerAt < 0 || textAt < 0 {
			t.Errorf("user message %q does not carry source %d (%s) and its text %q", user.Content, i+1, marker, text)
			continue
		}
		if markerAt <= last {
			t.Errorf("source %d appears out of retrieval order in %q", i+1, user.Content)
		}
		last = markerAt
	}
	if !strings.Contains(user.Content, "who won?") {
		t.Errorf("user message %q does not ask the question", user.Content)
	}
}

func TestNoRetrievedChunkAnswersWithoutCallingTheChatRole(t *testing.T) {
	retrieval := &stubAnswerRetrieval{result: &types.RetrievalResult{Results: []types.RetrievedChunk{}}}
	svc := NewAnswerService(retrieval, &forbiddenAnswerGenerator{t})

	got, err := svc.Answer(callerContext(uuid.New()), answerRequest("who won?", true))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if retrieval.calls != 1 {
		t.Errorf("retrieval called %d times, want once", retrieval.calls)
	}
	if got.Answer != noSourcesAnswer {
		t.Errorf("answer = %q, want the fixed no-source sentence %q", got.Answer, noSourcesAnswer)
	}
	if got.Citations == nil || len(got.Citations) != 0 {
		t.Errorf("citations = %+v, want an empty non-nil list", got.Citations)
	}
	if got.FinishReason != "" {
		t.Errorf("finish_reason = %q, want empty with no chat call", got.FinishReason)
	}
	if got.Debug == nil || len(got.Debug.Trace) != 0 || got.Debug.Usage != (types.TokenUsage{}) {
		t.Errorf("debug = %+v, want no steps and zero usage when no chat call was made", got.Debug)
	}
}

func TestDebugCarriesTheResultsTraceAndUsage(t *testing.T) {
	results := answerResults()
	trace := []types.TraceStep{
		{Step: types.TraceStepSearch, Technique: types.RetrievalTechniqueBM25, DurationMS: 4, Candidates: intPointer(len(results))},
	}
	usage := types.TokenUsage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}
	retrieval := &stubAnswerRetrieval{result: &types.RetrievalResult{Results: results, Trace: trace}}
	gen := &fakeGenerator{model: "test-chat", reply: &types.ChatResponse{Content: "Answer [1].", FinishReason: "stop", Usage: usage}}
	svc := NewAnswerService(retrieval, gen)

	got, err := svc.Answer(callerContext(uuid.New()), answerRequest("who won?", true))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if got.Debug == nil {
		t.Fatal("debug = nil, want the retrieved Chunks, trace and usage")
	}
	if !reflect.DeepEqual(got.Debug.Results, results) {
		t.Errorf("debug results = %+v, want the retrieved Chunks %+v", got.Debug.Results, results)
	}
	if got.Debug.Usage != usage {
		t.Errorf("debug usage = %+v, want %+v from the chat response", got.Debug.Usage, usage)
	}
	if len(got.Debug.Trace) != len(trace)+1 {
		t.Fatalf("debug trace = %+v, want the retrieval trace plus the generate step", got.Debug.Trace)
	}
	if !reflect.DeepEqual(got.Debug.Trace[:len(trace)], trace) {
		t.Errorf("debug trace starts with %+v, want the retrieval trace %+v", got.Debug.Trace, trace)
	}
	step := got.Debug.Trace[len(trace)]
	if step.Step != types.TraceStepGenerate || step.Model != gen.model || step.DurationMS < 0 || step.Technique != "" {
		t.Errorf("generate step = %+v, want the generate step naming %q with a duration and no technique", step, gen.model)
	}

	// Without debug the response carries no debug object at all.
	plain, err := svc.Answer(callerContext(uuid.New()), answerRequest("who won?", false))
	if err != nil {
		t.Fatalf("Answer without debug: %v", err)
	}
	if plain.Debug != nil {
		t.Errorf("debug = %+v, want nil when the request did not ask for it", plain.Debug)
	}
}

func TestTheAnswerSendsTheRetrievalConfigUnchangedToTheRetrievalService(t *testing.T) {
	results := answerResults()
	retrieval := &stubAnswerRetrieval{result: &types.RetrievalResult{Results: results}}
	svc := NewAnswerService(retrieval, &fakeGenerator{reply: &types.ChatResponse{Content: "Answer [1]."}})

	req := answerRequest("thue tncn", false)
	if _, err := svc.Answer(callerContext(uuid.New()), req); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	got := retrieval.got
	if got == nil {
		t.Fatal("retrieval was not called")
	}
	if got.Query != "thue tncn" {
		t.Errorf("query = %q, want the question", got.Query)
	}
	if got.TopK == nil || *got.TopK != 3 {
		t.Errorf("top_k = %v, want 3", got.TopK)
	}
	if got.RetrievalConfig == nil || len(got.RetrievalConfig.Techniques) != 2 ||
		got.RetrievalConfig.Techniques[0] != types.RetrievalTechniqueBM25 ||
		got.RetrievalConfig.Techniques[1] != types.RetrievalTechniqueDense {
		t.Errorf("retrieval_config = %+v, want bm25 and dense", got.RetrievalConfig)
	}
	if got.RetrievalConfig.RRFK == nil || *got.RetrievalConfig.RRFK != 10 {
		t.Errorf("rrf_k = %v, want 10", got.RetrievalConfig.RRFK)
	}
	if got.RetrievalConfig.RerankCandidates == nil || *got.RetrievalConfig.RerankCandidates != 7 {
		t.Errorf("rerank_candidates = %v, want 7", got.RetrievalConfig.RerankCandidates)
	}
}

func TestAnAnswerWithoutAChatModelIs503BeforeRetrieval(t *testing.T) {
	svc := NewAnswerService(&forbiddenAnswerRetrieval{t}, nil)

	_, err := svc.Answer(callerContext(uuid.New()), answerRequest("who won?", false))
	appErr, ok := apperrors.IsAppError(err)
	if !ok || appErr.HTTPCode != 503 || !strings.Contains(appErr.Message, "models.chat") {
		t.Errorf("Answer = %v, want a 503 error naming models.chat", err)
	}
}

func TestAnInvalidAnswerRequestIsValidation(t *testing.T) {
	svc := NewAnswerService(&forbiddenAnswerRetrieval{t}, &forbiddenAnswerGenerator{t})

	_, err := svc.Answer(callerContext(uuid.New()), &types.AnswerRequest{})
	appErr, ok := apperrors.IsAppError(err)
	if !ok || appErr.HTTPCode != 400 || appErr.Code != apperrors.ErrValidation || !strings.Contains(appErr.Message, "query") {
		t.Errorf("Answer = %v, want a validation error about the query", err)
	}
}

func TestRetrievalErrorsPassThroughUnchanged(t *testing.T) {
	want := apperrors.NewServiceUnavailableError("dense retrieval needs models.embedding to be configured")
	svc := NewAnswerService(&stubAnswerRetrieval{err: want}, &fakeGenerator{})

	_, err := svc.Answer(callerContext(uuid.New()), answerRequest("who won?", false))
	if err != want {
		t.Errorf("Answer = %v, want the retrieval error passed through unchanged", err)
	}
}

func TestAChatFailureFailsTheAnswer(t *testing.T) {
	cause := errors.New("provider refused")
	svc := NewAnswerService(
		&stubAnswerRetrieval{result: &types.RetrievalResult{Results: answerResults()}},
		&fakeGenerator{err: cause},
	)

	got, err := svc.Answer(callerContext(uuid.New()), answerRequest("who won?", false))
	if !errors.Is(err, cause) {
		t.Errorf("Answer = %v, want an error wrapping %v", err, cause)
	}
	if got != nil {
		t.Errorf("result = %+v, want nil when the chat role fails", got)
	}
}

// TestTheAnswerNeedsACaller guards the assumption every route behind the
// authentication middleware relies on.
func TestTheAnswerNeedsACaller(t *testing.T) {
	svc := NewAnswerService(&forbiddenAnswerRetrieval{t}, &forbiddenAnswerGenerator{t})

	if _, err := svc.Answer(context.Background(), answerRequest("who won?", false)); err == nil {
		t.Error("Answer without a Principal = nil, want an error")
	}
}

// The fakes satisfy the contracts they stand in for.
var (
	_ interfaces.RetrievalService = (*stubAnswerRetrieval)(nil)
	_ interfaces.Generator        = (*fakeGenerator)(nil)
)
