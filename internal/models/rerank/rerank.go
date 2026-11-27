// Package rerank exposes the Reranker interface used by the application
// layer and builds the client for a configured rerank role
// (models.rerank). Retrieval finds candidates cheaply and reranking orders
// them by how well they answer the query, so the client turns one
// /v1/rerank call into one relevance score per document, in the caller's
// order.
//
// The endpoint speaks the Jina/Cohere schema, where a provider numbers each
// result with the index of the document it scored and may answer in any
// order; the client puts the scores back next to the documents it sent.
package rerank

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/phongnd2802/trelis/internal/config"
	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/models/providererror"
)

// defaultPath is where a Jina/Cohere-compatible provider takes rerank
// requests, under the role's base URL.
const defaultPath = "/rerank"

// Reranker is the rerank model role.
type Reranker interface {
	// Rerank scores each document against query. scores[i] belongs to
	// documents[i]: the scores come back in the caller's order, not the
	// provider's.
	Rerank(ctx context.Context, query string, documents []string) ([]float64, error)

	// GetModelName returns the model name sent on the wire.
	GetModelName() string
}

// Option configures the client New builds.
type Option func(*client)

// WithRetry replaces the retry policy one call tries under.
func WithRetry(r api.Retry) Option {
	return func(c *client) { c.retry = r }
}

// client is the rerank role's client.
type client struct {
	endpoint api.Endpoint
	// retry is how one request is sent again after a failure worth waiting
	// out.
	retry api.Retry
	// timeout is the fallback deadline of the request, applied only when the
	// caller's ctx carries none.
	timeout time.Duration
}

// New builds the client for one configured rerank role (models.rerank).
func New(role config.RerankRole, opts ...Option) (Reranker, error) {
	endpoint, err := role.APIEndpoint("rerank")
	if err != nil {
		return nil, err
	}
	c := &client{
		endpoint: endpoint,
		retry:    api.DefaultRetry(),
		// Reranking is a non-streaming call, so the chat timeout is the right
		// fallback for one of them.
		timeout: api.DefaultChatTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// GetModelName returns the wire model name.
func (c *client) GetModelName() string { return c.endpoint.Model }

// Rerank scores each document against query, one score per document and in
// the order of documents. The caller's ctx bounds the call; a request whose
// ctx carries no deadline gets c.timeout of its own.
func (c *client) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	start := time.Now()
	var (
		scores []float64
		used   *usage
	)
	attempts, err := c.retry.DoWithAttempts(ctx, func(ctx context.Context) error {
		got, gotUsage, err := c.send(ctx, query, documents)
		if err != nil {
			return err
		}
		// Keep only the successful attempt's accounting: a retried attempt's
		// usage was not billed as part of this line.
		scores, used = got, gotUsage
		return nil
	})
	call := logger.ModelCall{
		Role:       "rerank",
		Model:      c.endpoint.Model,
		Protocol:   "rerank",
		Duration:   time.Since(start),
		Attempts:   attempts,
		Failed:     err != nil,
		HTTPStatus: api.HTTPStatusCode(err),
	}
	if used != nil {
		call.HasUsage = true
		call.InputTokens = used.TotalTokens
	}
	logger.LogModelCall(ctx, call)
	if err != nil {
		return nil, providererror.Wrap("rerank", err)
	}
	return scores, nil
}

// send posts one rerank request and returns its scores in the documents'
// order and the vendor's token accounting, when the reply carries it.
func (c *client) send(ctx context.Context, query string, documents []string) ([]float64, *usage, error) {
	body := requestBody{
		Model:     c.endpoint.Model,
		Query:     query,
		Documents: documents,
		// Some providers return fewer results than documents unless top_n asks
		// for them all; the caller needs a score for every candidate.
		TopN: len(documents),
		// The caller already has the documents, so only scores come back.
		ReturnDocuments: false,
	}
	raw, err := c.endpoint.PostJSON(ctx, defaultPath, body, c.timeout)
	if err != nil {
		return nil, nil, err
	}
	return scoresOf(raw, len(documents))
}

// requestBody is the Jina/Cohere rerank request.
type requestBody struct {
	Model           string   `json:"model"`
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	TopN            int      `json:"top_n"`
	ReturnDocuments bool     `json:"return_documents"`
}

// usage is the vendor's token accounting for one rerank request. A nil *usage
// means the reply carried none.
type usage struct {
	TotalTokens int `json:"total_tokens"`
}

// responseBody is the Jina/Cohere rerank reply.
type responseBody struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
	Usage *usage `json:"usage"`
	// Error carries the vendor's message when a gateway answers 200 with an
	// error instead of a status code.
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// scoresOf reads a reply to a request for want documents and puts its scores
// in the documents' order: a provider numbers each result with the index of
// the document it belongs to and may answer in any order. The usage pointer is
// nil when the reply carries no usage object.
func scoresOf(raw []byte, want int) ([]float64, *usage, error) {
	var body responseBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, nil, fmt.Errorf("decode response: %w", err)
	}
	if body.Error != nil && body.Error.Message != "" {
		return nil, nil, fmt.Errorf("API error: %s", body.Error.Message)
	}
	// A reply that does not carry one score per document is not an answer to
	// this request. It would come back the same way every time, so sending the
	// request again cannot help.
	if len(body.Results) != want {
		return nil, nil, fmt.Errorf("response carries %d results, want %d", len(body.Results), want)
	}
	scores := make([]float64, want)
	seen := make([]bool, want)
	for _, result := range body.Results {
		if result.Index < 0 || result.Index >= want || seen[result.Index] {
			return nil, nil, fmt.Errorf("response index %d is not one of 0..%d, or comes twice", result.Index, want-1)
		}
		// A score that is not a number cannot order anything.
		if math.IsNaN(result.RelevanceScore) || math.IsInf(result.RelevanceScore, 0) {
			return nil, nil, fmt.Errorf("score %d is not a finite number", result.Index)
		}
		seen[result.Index] = true
		scores[result.Index] = result.RelevanceScore
	}
	return scores, body.Usage, nil
}
