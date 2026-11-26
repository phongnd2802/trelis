// Package fakeprovider is an OpenAI-compatible model provider for tests. It
// serves Chat Completions, Responses, embeddings and rerank over real HTTP,
// so Trelis's model clients are exercised end to end, and it answers
// deterministically: the same request always gets the same content. Only
// response and tool call ids are counters, so they depend on request order.
//
// Chat Completions and Responses share one script. A test registers
// [Provider.Reply] rules; each request gets the reply of the first rule
// whose [Match] accepts it, rendered in that endpoint's wire format,
// streamed when the request asks for it. A generation request no rule
// matches fails the test. Embeddings and rerank need no script: they answer
// from the input text (see [Provider.EmbedAs] and [Provider.ScoreAs] to pin
// values). [Provider.FailNext] makes an endpoint return errors for a while,
// and [Provider.Requests] returns what each endpoint received.
package fakeprovider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Endpoint is a path under the provider's [Provider.URL].
type Endpoint string

// The endpoints the provider serves.
const (
	ChatCompletions Endpoint = "/chat/completions"
	Responses       Endpoint = "/responses"
	Embeddings      Endpoint = "/embeddings"
	Rerank          Endpoint = "/rerank"
)

// Provider is a running fake provider. Its methods are safe to call while
// requests are being served.
type Provider struct {
	t   testing.TB
	srv *httptest.Server

	mu       sync.Mutex
	rules    []rule
	failures map[Endpoint][]failure
	requests map[Endpoint][]Request
	vectors  map[string][]float32
	scores   map[[2]string]float64
	dims     int
	seq      int
}

// rule is one Reply registration.
type rule struct {
	match Match
	reply Reply
}

// failure is n queued answers with status.
type failure struct {
	status int
	n      int
}

// Request is one request an endpoint received.
type Request struct {
	Header http.Header
	// Body is the raw JSON body.
	Body []byte
}

// DefaultEmbeddingDims is the length of the vectors [Embeddings] returns
// unless [Provider.SetEmbeddingDims] changes it.
const DefaultEmbeddingDims = 8

// New starts a provider that is shut down when the test ends.
func New(t testing.TB) *Provider {
	t.Helper()
	p := &Provider{
		t:        t,
		failures: map[Endpoint][]failure{},
		requests: map[Endpoint][]Request{},
		vectors:  map[string][]float32{},
		scores:   map[[2]string]float64{},
		dims:     DefaultEmbeddingDims,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1"+string(ChatCompletions), p.serve(ChatCompletions, p.chatCompletions))
	mux.HandleFunc("POST /v1"+string(Responses), p.serve(Responses, p.responses))
	mux.HandleFunc("POST /v1"+string(Embeddings), p.serve(Embeddings, p.embeddings))
	mux.HandleFunc("POST /v1"+string(Rerank), p.serve(Rerank, p.rerank))
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// URL is the base URL a client is configured with, ending in /v1 and
// without a trailing slash: POST URL()+"/chat/completions".
func (p *Provider) URL() string { return p.srv.URL + "/v1" }

// Reply adds a rule: a Chat Completions or Responses request that match
// accepts, and no earlier rule did, is answered with r.
func (p *Provider) Reply(match Match, r Reply) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule{match: match, reply: r})
}

// FailNext makes the next n requests to e answer status with an
// OpenAI-style error body, before any scripted answer. Calls queue up in
// order, so FailNext(e, 429, 1) then FailNext(e, 500, 1) fails with 429,
// then 500, then answers.
func (p *Provider) FailNext(e Endpoint, status, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures[e] = append(p.failures[e], failure{status: status, n: n})
}

// Requests returns, in arrival order, every request e received, failed ones
// included.
func (p *Provider) Requests(e Endpoint) []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Request(nil), p.requests[e]...)
}

// serve records the request, applies a queued failure, then hands the
// decoded body to h. h returns the status and the JSON body to write, or
// writes a stream itself and returns 0.
func (p *Provider) serve(e Endpoint, h func(w http.ResponseWriter, body []byte) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		p.mu.Lock()
		p.requests[e] = append(p.requests[e], Request{Header: req.Header.Clone(), Body: body})
		status := p.takeFailure(e)
		p.mu.Unlock()
		if status != 0 {
			writeError(w, status, fmt.Sprintf("fakeprovider: scripted failure %d", status))
			return
		}
		if !json.Valid(body) {
			writeError(w, http.StatusBadRequest, "fakeprovider: body is not JSON")
			return
		}
		if status, out := h(w, body); status != 0 {
			writeJSON(w, status, out)
		}
	}
}

// takeFailure pops one queued failure for e, or returns 0. p.mu is held.
func (p *Provider) takeFailure(e Endpoint) int {
	q := p.failures[e]
	if len(q) == 0 {
		return 0
	}
	status := q[0].status
	if q[0].n--; q[0].n <= 0 {
		q = q[1:]
	}
	p.failures[e] = q
	return status
}

// generate picks the scripted reply for prompt, or fails the test.
func (p *Provider) generate(prompt Prompt) (Reply, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.rules {
		if r.match(prompt) {
			return r.reply, true
		}
	}
	p.t.Errorf("fakeprovider: no Reply rule matches this %s request:\n%s", prompt.Endpoint, prompt)
	return Reply{}, false
}

// nextID returns a fresh id with prefix. Ids are unique within a provider;
// they are the only part of an answer that depends on request order.
func (p *Provider) nextID(prefix string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	return fmt.Sprintf("%s_%d", prefix, p.seq)
}

// errorBody is the OpenAI error shape.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// newError is an error body with msg and typ.
func newError(msg, typ string) errorBody {
	var b errorBody
	b.Error.Message, b.Error.Type = msg, typ
	return b
}

// writeError answers status with msg, typed after the status text
// (too_many_requests, bad_gateway, ...).
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, newError(msg, strings.ReplaceAll(strings.ToLower(http.StatusText(status)), " ", "_")))
}

// writeJSON answers status with body as JSON.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// countTokens is the provider's stand-in tokenizer: one token per word, at
// least one.
func countTokens(s string) int {
	return max(1, len(strings.Fields(s)))
}
