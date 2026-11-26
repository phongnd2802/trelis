// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/endpoint.go.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AuthFunc stamps credentials onto an outbound request. body is the exact
// bytes being sent so signing vendors can hash them.
type AuthFunc func(req *http.Request, body []byte)

// BearerAuth is the default: Authorization: Bearer <key>. An empty key sends
// nothing (local deployments).
func BearerAuth(apiKey string) AuthFunc {
	return func(req *http.Request, _ []byte) {
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
}

// Endpoint is everything a protocol client needs to reach one model.
type Endpoint struct {
	// BaseURL is the vendor root, e.g. https://api.openai.com/v1.
	BaseURL string
	// Model is the name sent on the wire.
	Model string
	Auth  AuthFunc
	// Client is the HTTP client; nil uses HTTPClient.
	Client *http.Client
}

// Resolve builds the request URL from the base URL and the protocol path.
func (e Endpoint) Resolve(defaultPath string) string {
	return strings.TrimRight(e.BaseURL, "/") + defaultPath
}

func (e Endpoint) httpClient() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	return HTTPClient
}

// NewRequest prepares an authenticated JSON POST. The body is marshalled
// once so signing vendors see exactly what is sent.
func (e Endpoint) NewRequest(ctx context.Context, url string, body any, stream bool) (*http.Request, []byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if e.Auth != nil {
		e.Auth(req, data)
	}
	return req, data, nil
}

// Do sends the request and returns the response. Non-2xx responses become an
// HTTPError whose Body keeps the vendor's wording for callers that inspect it;
// Error() leaves the body out because error strings reach logs.
func (e Endpoint) Do(req *http.Request) (*http.Response, error) {
	resp, err := e.httpClient().Do(req)
	if err != nil {
		return nil, &TransportError{Op: "send request", Err: err}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(body), Header: resp.Header}
	}
	return resp, nil
}

// PostJSON sends body as one non-streaming JSON POST to path under BaseURL
// and returns the raw reply. It is the whole request a model client makes:
// one deadline (timeout, applied only when ctx carries none), one reply read
// with the shared cap, and a reply that stops halfway reported as a
// TransportError so the caller can send it again. An oversized body is a
// plain error instead, because the same request would get the same oversized
// reply every time.
func (e Endpoint) PostJSON(ctx context.Context, path string, body any, timeout time.Duration) ([]byte, error) {
	ctx, cancel := WithLLMTimeout(ctx, timeout)
	defer cancel()

	req, _, err := e.NewRequest(ctx, e.Resolve(path), body, false)
	if err != nil {
		return nil, err
	}
	resp, err := e.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return readReply(resp.Body, 0)
}

// readReply reads a reply body, refusing anything over limit (0 for the
// shared cap). A body past the cap is deterministic and comes back as a plain
// error; every other read failure is worth a retry and comes back as a
// TransportError.
func readReply(body io.Reader, limit int64) ([]byte, error) {
	raw, err := ReadResponseCapped(body, limit)
	if err != nil {
		if errors.Is(err, ErrResponseTooLarge) {
			return nil, fmt.Errorf("read response: %w", err)
		}
		return nil, &TransportError{Op: "read response", Err: err}
	}
	return raw, nil
}

// TransportError is a request that never got a whole answer: DNS, connect,
// TLS, a reset or a timeout, while sending or while reading the reply. It is
// the only failure worth sending again.
type TransportError struct {
	// Op is the phase that failed: "send request" or "read response".
	Op  string
	Err error
}

func (e *TransportError) Error() string { return e.Op + ": " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// HTTPError is a non-2xx vendor reply.
type HTTPError struct {
	StatusCode int
	Body       string
	// Header is the reply's headers, kept so a retry can honour the vendor's
	// Retry-After on a 429 or 503.
	Header http.Header
}

func (e *HTTPError) Error() string {
	// Body is deliberately absent: vendors echo request content in error
	// bodies, and this message reaches logs. Callers that need the vendor's
	// wording inspect Body themselves.
	return fmt.Sprintf("API request failed with status %d", e.StatusCode)
}

// HTTPStatusCode is the vendor status in err when it is an *HTTPError, or 0.
// Callers log it as http_status without putting the body-bearing error text
// in the log.
func HTTPStatusCode(err error) int {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode
	}
	return 0
}
