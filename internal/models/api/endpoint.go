// Package api holds what the protocol clients (Chat Completions, Responses,
// embeddings, rerank) share: the endpoint they call and how a failed call is
// reported.
package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AuthFunc authenticates a request; body is the encoded request body, for
// schemes that sign it.
type AuthFunc func(req *http.Request, body []byte)

// Endpoint is one OpenAI-compatible provider a model role calls.
type Endpoint struct {
	// BaseURL is the provider's API root, e.g. https://api.openai.com/v1.
	// Protocol clients append their path (/chat/completions, /embeddings ...).
	BaseURL string
	Model   string
	Auth    AuthFunc
	// Client defaults to http.DefaultClient.
	Client *http.Client
}

// BearerAuth sends key as a bearer token, or nothing when key is empty
// (local providers often need no key).
func BearerAuth(key string) AuthFunc {
	return func(req *http.Request, _ []byte) {
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
}

// Post sends body as JSON to the base URL plus path. A non-2xx answer is
// returned as *HTTPError with the response already closed; otherwise the
// caller closes the response body.
func (e Endpoint) Post(ctx context.Context, path string, body []byte) (*http.Response, error) {
	url := strings.TrimRight(e.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request to %s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.Auth != nil {
		e.Auth(req, body)
	}

	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, &HTTPError{URL: url, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	return resp, nil
}

// maxErrorBody bounds how much of an error response is kept.
const maxErrorBody = 2048

// HTTPError is a non-2xx answer from a provider. StatusCode lets callers
// decide whether to retry (429, 5xx).
type HTTPError struct {
	URL        string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s answered %d: %s", e.URL, e.StatusCode, e.Body)
}
