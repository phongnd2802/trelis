package rerank

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/phongnd2802/trelis/internal/config"
	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

// The fake provider answers /v1/rerank from the request's own query and
// documents and sorts its reply by score, so these tests read what the client
// sent as well as what came back.

const (
	model = "jina-reranker-v2-base-multilingual"
	key   = "sk-rerank"
	// attempts is how often a request is sent in the tests. The delays are
	// tiny: what is under test is the retrying, not the waiting.
	attempts = 3
)

func role(baseURL, model, key string) config.RerankRole {
	return config.RerankRole{
		Endpoint: config.Endpoint{BaseURL: baseURL, Model: model, APIKey: key},
	}
}

// reranker builds a client for p that retries fast.
func reranker(t *testing.T, p *modeltest.FakeProvider, opts ...Option) Reranker {
	t.Helper()
	r, err := New(role(p.URL(), model, key),
		append([]Option{WithRetry(api.Retry{
			MaxAttempts:   attempts,
			BaseDelay:     time.Millisecond,
			MaxDelay:      2 * time.Millisecond,
			MaxRetryAfter: time.Second,
		})}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// request is the JSON body of one rerank request.
type request struct {
	Model           string   `json:"model"`
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	TopN            int      `json:"top_n"`
	ReturnDocuments bool     `json:"return_documents"`
}

func decodeRequest(t *testing.T, r modeltest.Request) request {
	t.Helper()
	var req request
	if err := json.Unmarshal(r.Body, &req); err != nil {
		t.Fatalf("decode request body %s: %v", r.Body, err)
	}
	return req
}

func TestRerankSendsTheRolesModelAndKey(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.ScoreAs("thuế đất", "Luật Đất đai", 0.9)
	r := reranker(t, p)

	got, err := r.Rerank(context.Background(), "thuế đất", []string{"Luật Đất đai", "thời tiết"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Rerank = %v, want one score per document", got)
	}

	reqs := p.Requests(modeltest.Rerank)
	if len(reqs) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(reqs))
	}
	if auth := reqs[0].Header.Get("Authorization"); auth != "Bearer "+key {
		t.Errorf("Authorization = %q, want Bearer %s", auth, key)
	}
	req := decodeRequest(t, reqs[0])
	if req.Model != model || req.Query != "thuế đất" ||
		!slices.Equal(req.Documents, []string{"Luật Đất đai", "thời tiết"}) {
		t.Errorf("request = %+v, want model %q, the query and the documents", req, model)
	}
	// top_n is explicit because some providers default to returning fewer
	// results, and return_documents is false: the caller already has them.
	if req.TopN != 2 || req.ReturnDocuments {
		t.Errorf("request = %+v, want top_n 2 and return_documents false", req)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(reqs[0].Body, &fields); err != nil {
		t.Fatalf("request body %s: %v", reqs[0].Body, err)
	}
	if len(fields) != 5 {
		t.Errorf("request body = %s, want only model, query, documents, top_n and return_documents", reqs[0].Body)
	}
	if string(fields["return_documents"]) != "false" {
		t.Errorf("return_documents = %s, want false rather than left out", fields["return_documents"])
	}
	if r.GetModelName() != model {
		t.Errorf("GetModelName = %q, want %q", r.GetModelName(), model)
	}
}

func TestRerankKeepsScoresInTheDocumentsOrder(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	// Pinned out of order: the fake sorts its reply by score, so only the
	// reply's index can put each score back next to its document.
	p.ScoreAs("q", "a", 0.2)
	p.ScoreAs("q", "b", 0.9)
	p.ScoreAs("q", "c", 0.5)
	r := reranker(t, p)

	got, err := r.Rerank(context.Background(), "q", []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []float64{0.2, 0.9, 0.5}; !slices.Equal(got, want) {
		t.Errorf("Rerank = %v, want %v in the documents' order, not the reply's", got, want)
	}
}

func TestRerankRetriesATransientFailure(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			p := modeltest.NewFakeProvider(t)
			p.FailNext(modeltest.Rerank, status, 1)
			r := reranker(t, p)

			if _, err := r.Rerank(context.Background(), "q", []string{"a"}); err != nil {
				t.Fatalf("Rerank: %v", err)
			}
			if n := p.CallCount(modeltest.Rerank); n != 2 {
				t.Errorf("provider saw %d requests, want the request sent again after the %d", n, status)
			}
		})
	}
}

func TestRerankGivesUpAfterTheLastAttempt(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.FailNext(modeltest.Rerank, http.StatusInternalServerError, attempts+2)
	r := reranker(t, p)

	_, err := r.Rerank(context.Background(), "q", []string{"a"})
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Rerank err = %v, want the last %d", err, http.StatusInternalServerError)
	}
	if n := p.CallCount(modeltest.Rerank); n != attempts {
		t.Errorf("provider saw %d requests, want %d attempts", n, attempts)
	}
}

func TestRerankA429Is502OnlyAfterTheRetriesAreUsedUp(t *testing.T) {
	t.Run("every attempt is refused", func(t *testing.T) {
		p := modeltest.NewFakeProvider(t)
		p.FailNext(modeltest.Rerank, http.StatusTooManyRequests, attempts)
		r := reranker(t, p)

		_, err := r.Rerank(context.Background(), "q", []string{"a"})
		appErr, ok := apperrors.IsAppError(err)
		if !ok || appErr.Code != apperrors.ErrModelProvider || appErr.HTTPCode != http.StatusBadGateway {
			t.Fatalf("Rerank err = %v, want the 502 ErrModelProvider after the retries", err)
		}
		if want := "the rerank model provider rejected the call (HTTP 429)"; appErr.Message != want {
			t.Errorf("message = %q, want %q", appErr.Message, want)
		}
		if n := p.CallCount(modeltest.Rerank); n != attempts {
			t.Errorf("provider saw %d requests, want %d attempts before the 502", n, attempts)
		}
	})

	t.Run("an attempt gets through", func(t *testing.T) {
		p := modeltest.NewFakeProvider(t)
		p.FailNext(modeltest.Rerank, http.StatusTooManyRequests, attempts-1)
		r := reranker(t, p)

		if _, err := r.Rerank(context.Background(), "q", []string{"a"}); err != nil {
			t.Fatalf("Rerank: %v after a retry got through", err)
		}
		if n := p.CallCount(modeltest.Rerank); n != attempts {
			t.Errorf("provider saw %d requests, want %d attempts", n, attempts)
		}
	})
}

func TestRerankSurfacesAClientErrorWithoutRetrying(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.FailNext(modeltest.Rerank, http.StatusBadRequest, attempts)
	r := reranker(t, p)

	_, err := r.Rerank(context.Background(), "q", []string{"a"})
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("Rerank err = %v, want the %d as an *api.HTTPError", err, http.StatusBadRequest)
	}
	if n := p.CallCount(modeltest.Rerank); n != 1 {
		t.Errorf("provider saw %d requests, want 1: sending a bad request again cannot fix it", n)
	}
}

func TestRerankWithoutDocumentsSendsNothing(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	r := reranker(t, p)

	got, err := r.Rerank(context.Background(), "q", nil)
	if err != nil || len(got) != 0 {
		t.Errorf("Rerank(nil) = %v, %v, want no scores and no error", got, err)
	}
	if n := p.CallCount(modeltest.Rerank); n != 0 {
		t.Errorf("provider saw %d requests, want none", n)
	}
}

func TestNewRejectsAnIncompleteRole(t *testing.T) {
	cases := map[string]config.RerankRole{
		"unconfigured":   {},
		"no base URL":    role("", model, key),
		"no model":       role("http://localhost:4000/v1", "", key),
		"blank base URL": role("   ", model, key),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := New(r)
			if err == nil {
				t.Fatalf("New(%+v) = %v, want an error", r, got)
			}
			// The message names the role, so a caller wiring several of them
			// together can tell which one is incomplete.
			if !strings.Contains(err.Error(), "rerank:") {
				t.Errorf("New err = %q, want it to name the rerank role", err)
			}
		})
	}
}

// stub answers every POST /v1/rerank with reply and counts what it served. It
// stands in for the providers the fake does not imitate: ones that answer
// with the wrong number of results, a bad index, or an error at 200.
func stub(t *testing.T, reply any) (string, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", calls
}

// result is one "results" entry of a rerank reply.
func result(index int, score float64) map[string]any {
	return map[string]any{"index": index, "relevance_score": score}
}

func TestRerankRejectsAReplyThatDoesNotMatchTheDocuments(t *testing.T) {
	cases := map[string]any{
		"too few results":    map[string]any{"results": []any{result(0, 0.9)}},
		"index out of range": map[string]any{"results": []any{result(0, 0.9), result(5, 0.1)}},
		"repeated index":     map[string]any{"results": []any{result(0, 0.9), result(0, 0.1)}},
		"no results":         map[string]any{"model": model},
		"error at 200":       map[string]any{"error": map[string]any{"message": "input too long"}},
		"invalid JSON":       json.RawMessage(`{"results":[`),
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			base, calls := stub(t, reply)
			r, err := New(role(base, model, key), WithRetry(api.Retry{
				MaxAttempts: attempts, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond,
			}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Rerank(context.Background(), "q", []string{"a", "b"})
			if err == nil {
				t.Fatal("Rerank succeeded, want an error")
			}
			if name == "error at 200" && !strings.Contains(err.Error(), "input too long") {
				t.Errorf("Rerank err = %v, want the provider's message", err)
			}
			// A reply that does not match the request is the same every time,
			// so sending it again cannot help.
			if n := calls.Load(); n != 1 {
				t.Errorf("provider saw %d requests, want 1", n)
			}
		})
	}
}

// The fake's connection closes mid-body; that is the one read failure a second
// send can fix, so it is worth a retry.
func TestRerankRetriesAReplyThatStopsHalfway(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"results":[`)
	}))
	t.Cleanup(srv.Close)

	r, err := New(role(srv.URL+"/v1", model, key), WithRetry(api.Retry{
		MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond,
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Rerank(context.Background(), "q", []string{"a"})
	var transportErr *api.TransportError
	if !errors.As(err, &transportErr) || transportErr.Op != "read response" {
		t.Fatalf("Rerank err = %v, want a *api.TransportError from reading the reply", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server saw %d requests, want the request sent again", n)
	}
}

func TestRerankLogsOneModelCallAndCountsRetries(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	p := modeltest.NewFakeProvider(t)
	p.FailNext(modeltest.Rerank, http.StatusInternalServerError, 1)
	r := reranker(t, p)

	if _, err := r.Rerank(context.Background(), "secret query", []string{"secret document"}); err != nil {
		t.Fatal(err)
	}

	entries := logs.FilterMessage("model call").All()
	if len(entries) != 1 {
		t.Fatalf("logger saw %d model call lines, want 1", len(entries))
	}
	entry := entries[0]
	fields := entry.ContextMap()
	want := map[string]any{
		"role":     "rerank",
		"model":    model,
		"protocol": "rerank",
		"status":   "ok",
		"attempts": int64(2),
		// The fake counts one token per word in the query and documents.
		"input_tokens": int64(4),
	}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("%s = %v, want %v", k, fields[k], v)
		}
	}
	if _, ok := fields["output_tokens"]; ok {
		t.Errorf("output_tokens = %v, want it omitted for rerank", fields["output_tokens"])
	}
	if s := fmt.Sprint(entry); strings.Contains(s, "secret query") || strings.Contains(s, "secret document") {
		t.Errorf("the model call log carries the query or a document: %s", s)
	}
}

// A reply without a usage object logs no token fields at all.
func TestRerankLogsNoTokensWhenUsageIsAbsent(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	base, _ := stub(t, map[string]any{"results": []any{result(0, 0.9)}})
	r, err := New(role(base, model, key), WithRetry(api.Retry{MaxAttempts: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Rerank(context.Background(), "secret query", []string{"secret document"}); err != nil {
		t.Fatal(err)
	}

	entries := logs.FilterMessage("model call").All()
	if len(entries) != 1 {
		t.Fatalf("logger saw %d model call lines, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if _, ok := fields["input_tokens"]; ok {
		t.Errorf("input_tokens = %v, want it omitted without usage", fields["input_tokens"])
	}
	if _, ok := fields["output_tokens"]; ok {
		t.Errorf("output_tokens = %v, want it omitted without usage", fields["output_tokens"])
	}
}
