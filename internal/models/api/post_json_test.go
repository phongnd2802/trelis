package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// postJSONServer serves every request with h under /v1, the way a model
// provider does.
func postJSONServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func TestPostJSONSendsTheBodyAndReturnsTheReply(t *testing.T) {
	var got map[string]string
	base := postJSONServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	raw, err := Endpoint{BaseURL: base}.PostJSON(
		context.Background(), "/echo", map[string]string{"hi": "there"}, time.Second)
	if err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if string(raw) != `{"ok":true}` {
		t.Errorf("PostJSON = %s, want the reply as it came", raw)
	}
	if got["hi"] != "there" {
		t.Errorf("request body = %v, want the map sent", got)
	}
}

// The fallback deadline covers a caller that set none of its own.
func TestPostJSONAppliesTheTimeoutWithoutACallerDeadline(t *testing.T) {
	base := postJSONServer(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, `{}`)
	})

	_, err := Endpoint{BaseURL: base}.PostJSON(
		context.Background(), "/slow", map[string]any{}, 10*time.Millisecond)
	var transportErr *TransportError
	if !errors.As(err, &transportErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("PostJSON err = %v, want a *TransportError from the fallback deadline", err)
	}
}

// A caller's own deadline wins over the fallback, longer or shorter.
func TestPostJSONKeepsACallersOwnDeadline(t *testing.T) {
	base := postJSONServer(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(w, `{}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Applying the 1ms fallback would cut this reply off.
	if _, err := (Endpoint{BaseURL: base}).PostJSON(ctx, "/slow", map[string]any{}, time.Millisecond); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
}

// A reply that stops halfway is worth sending the request again.
func TestPostJSONReportsATruncatedReplyAsATransportError(t *testing.T) {
	base := postJSONServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Promise a longer body than is written and let the server close the
		// connection: the client gets a reply that never arrived whole.
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":`)
	})

	_, err := Endpoint{BaseURL: base}.PostJSON(
		context.Background(), "/short", map[string]any{}, time.Second)
	var transportErr *TransportError
	if !errors.As(err, &transportErr) || transportErr.Op != "read response" {
		t.Errorf("PostJSON err = %v, want a *TransportError from reading the reply", err)
	}
}

func TestPostJSONReturnsANon2xxAsAnHTTPError(t *testing.T) {
	base := postJSONServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
	})

	_, err := Endpoint{BaseURL: base}.PostJSON(
		context.Background(), "/bad", map[string]any{}, time.Second)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("PostJSON err = %v, want the %d as an *HTTPError", err, http.StatusBadRequest)
	}
	if !strings.Contains(httpErr.Body, "nope") {
		t.Errorf("HTTPError body = %q, want the provider's message", httpErr.Body)
	}
}

// An oversized body is the same size every time, so it must not be a
// TransportError a client would send again. Serving 64 MB is impractical, so
// the cap is exercised through readReply, the step PostJSON reads with.
func TestReadReplyTreatsAnOversizedBodyAsDeterministic(t *testing.T) {
	_, err := readReply(strings.NewReader("12345"), 4)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("readReply err = %v, want ErrResponseTooLarge", err)
	}
	var transportErr *TransportError
	if errors.As(err, &transportErr) {
		t.Errorf("readReply err = %v, want no *TransportError for an oversized body", err)
	}
}
