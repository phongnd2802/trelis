package embedding

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

	"github.com/phongnd2802/trelis/internal/config"
	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

// The fake provider answers /v1/embeddings from the request's own texts, so
// these tests read what the client sent as well as what came back.

const (
	model = "text-embedding-3-small"
	key   = "sk-embed"
	// attempts is how often a batch is sent in the tests. The delays are
	// tiny: what is under test is the retrying, not the waiting.
	attempts = 3
)

func role(baseURL, model, key string, batchSize int) config.EmbeddingRole {
	return config.EmbeddingRole{
		BatchSize: batchSize,
		Endpoint:  config.Endpoint{BaseURL: baseURL, Model: model, APIKey: key},
	}
}

// embedder builds a client for p that retries fast.
func embedder(t *testing.T, p *modeltest.FakeProvider, batchSize int, opts ...Option) Embedder {
	t.Helper()
	e, err := New(role(p.URL(), model, key, batchSize),
		append([]Option{WithRetry(attempts, time.Millisecond, 2*time.Millisecond)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// request is the JSON body of one embeddings request.
type request struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

func decodeRequest(t *testing.T, r modeltest.Request) request {
	t.Helper()
	var req request
	if err := json.Unmarshal(r.Body, &req); err != nil {
		t.Fatalf("decode request body %s: %v", r.Body, err)
	}
	return req
}

// sameVectors compares two lists of vectors: a slice is not comparable, so
// slices.Equal cannot take one as an element.
func sameVectors(got, want [][]float32) bool {
	return slices.EqualFunc(got, want, func(a, b []float32) bool { return slices.Equal(a, b) })
}

func TestEmbedSendsTheRolesModelAndKey(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.EmbedAs("thuế đất", []float32{1, 0, 0})
	e := embedder(t, p, 8)

	got, err := e.Embed(context.Background(), []string{"thuế đất"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !slices.Equal(got[0], []float32{1, 0, 0}) {
		t.Errorf("Embed = %v, want the pinned vector", got)
	}

	reqs := p.Requests(modeltest.Embeddings)
	if len(reqs) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(reqs))
	}
	if auth := reqs[0].Header.Get("Authorization"); auth != "Bearer "+key {
		t.Errorf("Authorization = %q, want Bearer %s", auth, key)
	}
	if req := decodeRequest(t, reqs[0]); req.Model != model || !slices.Equal(req.Input, []string{"thuế đất"}) {
		t.Errorf("request = %+v, want model %q and the one text", req, model)
	}
	// The vectors come back as float arrays: no encoding_format, and the
	// role's model decides the length, so no dimensions either.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(reqs[0].Body, &fields); err != nil {
		t.Fatalf("request body %s: %v", reqs[0].Body, err)
	}
	if len(fields) != 2 || fields["model"] == nil || fields["input"] == nil {
		t.Errorf("request body = %s, want only model and input", reqs[0].Body)
	}
	if e.GetModelName() != model {
		t.Errorf("GetModelName = %q, want %q", e.GetModelName(), model)
	}
}

func TestEmbedSplitsInputIntoBatchesAndKeepsTheOrder(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	texts := []string{"một", "hai", "ba", "bốn", "năm"}
	for i, text := range texts {
		p.EmbedAs(text, []float32{float32(i)})
	}
	e := embedder(t, p, 2)

	got, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]float32{{0}, {1}, {2}, {3}, {4}}
	if !sameVectors(got, want) {
		t.Errorf("Embed = %v, want %v: every text keeps its own vector", got, want)
	}

	reqs := p.Requests(modeltest.Embeddings)
	if len(reqs) != 3 {
		t.Fatalf("provider saw %d requests, want 3: batches of 2, 2 and 1", len(reqs))
	}
	var sizes []int
	var sent []string
	for _, r := range reqs {
		req := decodeRequest(t, r)
		sizes = append(sizes, len(req.Input))
		sent = append(sent, req.Input...)
		if req.Model != model {
			t.Errorf("request = %+v, want model %q", req, model)
		}
	}
	if want := []int{2, 2, 1}; !slices.Equal(sizes, want) {
		t.Errorf("batch sizes = %v, want %v", sizes, want)
	}
	if !slices.Equal(sent, texts) {
		t.Errorf("texts sent = %v, want %v in the caller's order", sent, texts)
	}
}

func TestEmbedFallsBackToTheDefaultBatchSize(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	e := embedder(t, p, 0)
	texts := make([]string, config.DefaultEmbeddingBatchSize+1)
	for i := range texts {
		texts[i] = fmt.Sprintf("văn bản %d", i)
	}

	if _, err := e.Embed(context.Background(), texts); err != nil {
		t.Fatal(err)
	}
	if n := p.CallCount(modeltest.Embeddings); n != 2 {
		t.Errorf("provider saw %d requests, want %d texts sent as %d plus 1",
			n, len(texts), config.DefaultEmbeddingBatchSize)
	}
}

func TestEmbedRetriesTransientFailures(t *testing.T) {
	for _, status := range []int{
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			p := modeltest.NewFakeProvider(t)
			p.FailNext(modeltest.Embeddings, status, 1)
			e := embedder(t, p, 8)

			if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
				t.Fatalf("Embed: %v", err)
			}
			if n := p.CallCount(modeltest.Embeddings); n != 2 {
				t.Errorf("provider saw %d requests, want the batch sent again after the %d", n, status)
			}
		})
	}
}

func TestEmbedRetriesAReplyThatStopsHalfway(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		// Promise a longer body than is written and let the server close the
		// connection: the client gets a reply that never arrived whole.
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[`))
	}))
	t.Cleanup(srv.Close)

	e, err := New(role(srv.URL+"/v1", model, key, 8), WithRetry(2, time.Millisecond, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Embed(context.Background(), []string{"x"})
	var transportErr *api.TransportError
	if !errors.As(err, &transportErr) || transportErr.Op != "read response" {
		t.Fatalf("Embed err = %v, want a *api.TransportError from reading the reply", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server saw %d requests, want the batch sent again", n)
	}
}

func TestEmbedGivesUpAfterTheLastAttempt(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.FailNext(modeltest.Embeddings, http.StatusInternalServerError, attempts+2)
	e := embedder(t, p, 8)

	_, err := e.Embed(context.Background(), []string{"x"})
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Embed err = %v, want the last %d", err, http.StatusInternalServerError)
	}
	if n := p.CallCount(modeltest.Embeddings); n != attempts {
		t.Errorf("provider saw %d requests, want %d attempts", n, attempts)
	}
}

func TestEmbedStopsWhenRetryAfterIsLongerThanItWillWait(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "600")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limit reached","type":"rate_limit_exceeded","code":"429"}}`)
	}))
	t.Cleanup(srv.Close)

	e, err := New(role(srv.URL+"/v1", model, key, 8),
		WithRetry(3, time.Millisecond, 2*time.Millisecond), WithMaxRetryAfter(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Embed(context.Background(), []string{"x"})
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("Embed err = %v, want the provider's own %d", err, http.StatusTooManyRequests)
	}
	// Ten minutes is not a wait this client serves, and coming back after 1ms
	// would only be told the same thing, so the provider's own answer goes
	// back to the caller on the first attempt.
	if n := calls.Load(); n != 1 {
		t.Errorf("server saw %d requests, want 1", n)
	}
}

func TestEmbedSurfacesAClientErrorWithoutRetrying(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.FailNext(modeltest.Embeddings, http.StatusBadRequest, attempts)
	e := embedder(t, p, 8)

	_, err := e.Embed(context.Background(), []string{"x"})
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("Embed err = %v, want the %d as an *api.HTTPError", err, http.StatusBadRequest)
	}
	if n := p.CallCount(modeltest.Embeddings); n != 1 {
		t.Errorf("provider saw %d requests, want 1: sending a bad request again cannot fix it", n)
	}
	if !strings.Contains(err.Error(), "texts 1-1") {
		t.Errorf("Embed err = %v, want it to name the batch that failed", err)
	}
}

func TestEmbedWithoutTextsSendsNothing(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	e := embedder(t, p, 8)

	got, err := e.Embed(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("Embed(nil) = %v, %v, want no vectors and no error", got, err)
	}
	if n := p.CallCount(modeltest.Embeddings); n != 0 {
		t.Errorf("provider saw %d requests, want none", n)
	}
}

// cancelAfter cancels a context once n requests have gone out, so a retry loop
// is interrupted mid-flight instead of on a timer.
type cancelAfter struct {
	rt     http.RoundTripper
	n      int
	seen   int
	cancel context.CancelFunc
}

func (c *cancelAfter) RoundTrip(req *http.Request) (*http.Response, error) {
	if c.seen++; c.seen >= c.n {
		c.cancel()
	}
	return c.rt.RoundTrip(req)
}

func TestEmbedStopsRetryingWhenTheContextIsCancelled(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.FailNext(modeltest.Embeddings, http.StatusInternalServerError, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := embedder(t, p, 8,
		WithHTTPClient(&http.Client{Transport: &cancelAfter{rt: http.DefaultTransport, n: 2, cancel: cancel}}),
		WithRetry(10, time.Millisecond, time.Millisecond))

	if _, err := e.Embed(ctx, []string{"x"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Embed err = %v, want context.Canceled", err)
	}
	if n := p.CallCount(modeltest.Embeddings); n != 1 {
		t.Errorf("provider saw %d requests, want the loop to stop once the context went", n)
	}
}

func TestEmbedWithACancelledContextSendsNothing(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	e := embedder(t, p, 8)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Embed(ctx, []string{"x"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Embed err = %v, want context.Canceled", err)
	}
	if n := p.CallCount(modeltest.Embeddings); n != 0 {
		t.Errorf("provider saw %d requests, want none", n)
	}
}

func TestNewRejectsAnIncompleteRole(t *testing.T) {
	cases := map[string]config.EmbeddingRole{
		"unconfigured":   {},
		"no base URL":    role("", model, key, 8),
		"no model":       role("http://localhost:4000/v1", "", key, 8),
		"blank base URL": role("   ", model, key, 8),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			e, err := New(r)
			if err == nil {
				t.Fatalf("New(%+v) = %v, want an error", r, e)
			}
			// The message names the role, so a caller wiring several of them
			// together can tell which one is incomplete.
			if !strings.Contains(err.Error(), "embedding:") {
				t.Errorf("New err = %q, want it to name the embedding role", err)
			}
		})
	}
}

func TestNewAcceptsARoleWithoutAKey(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	e, err := New(role(p.URL(), model, "", 8))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if auth := p.Requests(modeltest.Embeddings)[0].Header.Get("Authorization"); auth != "" {
		t.Errorf("Authorization = %q, want none for a role without a key", auth)
	}
}

// stub answers every POST /v1/embeddings with out(input) and counts what it
// served. It stands in for the providers the fake does not imitate: ones that
// answer out of order or with the wrong number of vectors.
func stub(t *testing.T, out func(input []string) any) (string, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.Unmarshal(raw, &req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out(req.Input))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", calls
}

// vector is one "data" entry of an embeddings reply.
func vector(index int, x float32) map[string]any {
	return map[string]any{"object": "embedding", "index": index, "embedding": []float32{x}}
}

func TestEmbedOrdersVectorsByTheResponseIndex(t *testing.T) {
	base, _ := stub(t, func(input []string) any {
		data := make([]map[string]any, len(input))
		for i, text := range input {
			// Answer backwards: index, not position, says which text a
			// vector belongs to.
			data[len(input)-1-i] = vector(i, float32(len(text)))
		}
		return map[string]any{"object": "list", "data": data}
	})
	e, err := New(role(base, model, key, 8), WithRetry(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}

	got, err := e.Embed(context.Background(), []string{"a", "bb", "ccc"})
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]float32{{1}, {2}, {3}}; !sameVectors(got, want) {
		t.Errorf("Embed = %v, want %v: the vectors in the input's order", got, want)
	}
}

func TestEmbedRejectsAResponseThatDoesNotMatchTheBatch(t *testing.T) {
	cases := map[string]any{
		"too few vectors":    map[string]any{"data": []any{vector(0, 1)}},
		"index out of range": map[string]any{"data": []any{vector(0, 1), vector(5, 2)}},
		"repeated index":     map[string]any{"data": []any{vector(0, 1), vector(0, 2)}},
		"no data":            map[string]any{"model": model},
		"empty vector": json.RawMessage(
			`{"data":[{"index":0,"embedding":[]},{"index":1,"embedding":[1]}]}`),
		"vectors of two lengths": json.RawMessage(
			`{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[1,2]}]}`),
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			base, calls := stub(t, func([]string) any { return reply })
			e, err := New(role(base, model, key, 8), WithRetry(attempts, time.Millisecond, time.Millisecond))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Embed(context.Background(), []string{"a", "b"}); err == nil {
				t.Fatal("Embed succeeded, want an error")
			}
			// A reply that does not match the request is the same every time,
			// so sending the batch again cannot help.
			if n := calls.Load(); n != 1 {
				t.Errorf("provider saw %d requests, want 1", n)
			}
		})
	}
}

func TestEmbedRejectsBatchesOfDifferentShapes(t *testing.T) {
	p := modeltest.NewFakeProvider(t)
	p.EmbedAs("a", []float32{1, 2})
	p.EmbedAs("b", []float32{1, 2, 3})
	e := embedder(t, p, 1)

	_, err := e.Embed(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("Embed succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "texts 2-2") {
		t.Errorf("Embed err = %v, want it to name the batch that changed shape", err)
	}
	// Both batches went out: the first one is what set the shape the second
	// was measured against.
	if n := p.CallCount(modeltest.Embeddings); n != 2 {
		t.Errorf("provider saw %d requests, want 2", n)
	}
}

func TestEmbedGivesEveryRequestItsOwnFallbackTimeout(t *testing.T) {
	base, calls := stub(t, func(input []string) any {
		// Slower than a quarter of the deadline under test, so four of these
		// overrun a budget over the whole Embed while each one fits a budget
		// over itself.
		time.Sleep(40 * time.Millisecond)
		data := make([]map[string]any, len(input))
		for i, text := range input {
			data[i] = vector(i, float32(len(text)))
		}
		return map[string]any{"data": data}
	})
	e, err := New(role(base, model, key, 1), WithRetry(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	// The fallback is unexported and has no option of its own: this is the one
	// way to make it small enough for a test to overrun.
	e.(*client).timeout = 100 * time.Millisecond

	if _, err := e.Embed(context.Background(), []string{"a", "bb", "ccc", "dddd"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if n := calls.Load(); n != 4 {
		t.Errorf("server saw %d requests, want 4: none of them cut off by the others", n)
	}
}

func TestEmbedRespectsTheCallersOwnDeadline(t *testing.T) {
	base, _ := stub(t, func(input []string) any {
		time.Sleep(200 * time.Millisecond)
		data := make([]map[string]any, len(input))
		for i, text := range input {
			data[i] = vector(i, float32(len(text)))
		}
		return map[string]any{"data": data}
	})
	e, err := New(role(base, model, key, 8), WithRetry(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := e.Embed(ctx, []string{"x"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Embed err = %v, want context.DeadlineExceeded", err)
	}
}
