// Package modeltest holds test helpers for code that calls models. Its
// FakeProvider is an in-process stand-in for an OpenAI-compatible provider:
// it serves Chat Completions, Responses, embeddings and rerank over real
// HTTP, so Trelis's model clients are exercised end to end, and it answers
// deterministically. Only response and tool call ids are counters, so they
// depend on request order.
//
// Build tag is intentionally absent: other packages import this from their
// tests (and the integration suite). Keep production code out. (Wording
// from Omnisc/WeKnora cli/internal/testutil, Copyright (C) 2026 Tencent,
// MIT license; see NOTICE.)
//
// Chat Completions and Responses share one script, rendered in each
// endpoint's wire format and streamed when the request asks for it:
//
//   - [FakeProvider.Script] replays a fixed sequence of replies, one per
//     call its [Match] accepts, the way an agent loop consumes them. The
//     Match keeps other callers (ingest, Entity Resolution) from taking a
//     turn meant for the loop.
//   - [FakeProvider.Reply] rules answer any request their Match accepts,
//     when no script does; the first matching rule wins.
//
// A generation request that neither answers fails the test: an unexpected
// extra call. (The replay and that wording follow Omnisc/WeKnora's
// scriptedTemplateChatModel, Copyright (C) 2026 Tencent, MIT license; see
// NOTICE.) Embeddings and rerank need no script: they answer from the
// input text ([FakeProvider.EmbedAs] and [FakeProvider.ScoreAs] pin values).
// [FakeProvider.FailNext] makes an endpoint return errors for a while,
// [FakeProvider.Delay] holds a request in flight, and
// [FakeProvider.CallCount] and [FakeProvider.Requests] report what each
// endpoint received.
package modeltest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Endpoint is a path under the provider's [FakeProvider.URL].
type Endpoint string

// The endpoints the provider serves.
const (
	ChatCompletions Endpoint = "/chat/completions"
	Responses       Endpoint = "/responses"
	Embeddings      Endpoint = "/embeddings"
	Rerank          Endpoint = "/rerank"
)

// FakeProvider is an in-process stand-in for an OpenAI-compatible model
// provider. Its methods are safe to call while requests are being served.
type FakeProvider struct {
	t   testing.TB
	srv *httptest.Server

	mu       sync.Mutex
	scripts  []script
	rules    []rule
	failures map[Endpoint][]failure
	delays   map[Endpoint]time.Duration
	requests map[Endpoint][]Request
	vectors  map[string][]float32
	scores   map[[2]string]float64
	dims     int
	seq      int
}

// script is one Script registration; replies still to be given.
type script struct {
	match   Match
	replies []Reply
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
// unless [FakeProvider.SetEmbeddingDims] changes it.
const DefaultEmbeddingDims = 8

// NewFakeProvider starts a provider that is shut down when the test ends.
func NewFakeProvider(t testing.TB) *FakeProvider {
	t.Helper()
	p := &FakeProvider{
		t:        t,
		failures: map[Endpoint][]failure{},
		delays:   map[Endpoint]time.Duration{},
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
func (p *FakeProvider) URL() string { return p.srv.URL + "/v1" }

// Reply adds a rule: a Chat Completions or Responses request that match
// accepts, and no earlier rule did, is answered with r.
func (p *FakeProvider) Reply(match Match, r Reply) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule{match: match, reply: r})
}

// Script queues replies that the next Chat Completions or Responses calls
// match accepts get, one per call and in order, before any
// [FakeProvider.Reply] rule is consulted. Scripts are tried in the order
// they were added; a used-up script no longer answers.
func (p *FakeProvider) Script(match Match, replies ...Reply) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripts = append(p.scripts, script{match: match, replies: replies})
}

// FailNext makes the next n requests to e answer status with an
// OpenAI-style error body, before any scripted answer. Calls queue up in
// order, so FailNext(e, 429, 1) then FailNext(e, 500, 1) fails with 429,
// then 500, then answers.
func (p *FakeProvider) FailNext(e Endpoint, status, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures[e] = append(p.failures[e], failure{status: status, n: n})
}

// Delay makes every request to e wait d before it is answered, so a test can
// catch a call in flight. A request whose client cancels stops the wait and
// gets no answer. Zero removes the delay.
func (p *FakeProvider) Delay(e Endpoint, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delays[e] = d
}

// CallCount is how many requests e received, failed ones included.
func (p *FakeProvider) CallCount(e Endpoint) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests[e])
}

// Requests returns, in arrival order, every request e received, failed ones
// included.
func (p *FakeProvider) Requests(e Endpoint) []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Request(nil), p.requests[e]...)
}

// serve records the request, applies a queued failure, then hands the
// decoded body to h. h returns the status and the JSON body to write, or
// writes a stream itself and returns 0.
func (p *FakeProvider) serve(e Endpoint, h func(w http.ResponseWriter, body []byte) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		p.mu.Lock()
		p.requests[e] = append(p.requests[e], Request{Header: req.Header.Clone(), Body: body})
		status := p.takeFailure(e)
		delay := p.delays[e]
		p.mu.Unlock()
		if status != 0 {
			writeError(w, status, fmt.Sprintf("modeltest: scripted failure %d", status))
			return
		}
		if delay > 0 {
			select {
			case <-req.Context().Done():
				return
			case <-time.After(delay):
			}
		}
		if !json.Valid(body) {
			writeError(w, http.StatusBadRequest, "modeltest: body is not JSON")
			return
		}
		if status, out := h(w, body); status != 0 {
			writeJSON(w, status, out)
		}
	}
}

// takeFailure pops one queued failure for e, or returns 0. p.mu is held.
func (p *FakeProvider) takeFailure(e Endpoint) int {
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

// generate takes the next reply of the first script that matches prompt,
// else the first matching rule's, else fails the test.
func (p *FakeProvider) generate(prompt Prompt) (Reply, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, s := range p.scripts {
		if len(s.replies) > 0 && s.match(prompt) {
			p.scripts[i].replies = s.replies[1:]
			return s.replies[0], true
		}
	}
	for _, r := range p.rules {
		if r.match(prompt) {
			return r.reply, true
		}
	}
	p.t.Errorf("modeltest: unexpected extra call: no scripted reply is left and no Reply rule matches this %s request:\n%s", prompt.Endpoint, prompt)
	return Reply{}, false
}

// nextID returns a fresh id with prefix. Ids are unique within a provider;
// they are the only part of an answer that depends on request order.
func (p *FakeProvider) nextID(prefix string) string {
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
