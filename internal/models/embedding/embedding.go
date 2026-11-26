// Package embedding exposes the Embedder interface used by the application
// layer and builds the client for a configured embedding role
// (models.embedding). A provider embeds a whole batch of texts per request
// and rate-limits callers that send too much at once, so the client splits
// texts into batches and sends a batch again when the provider reports a
// failure worth waiting out.
//
// Ingest runs these calls as Temporal activities that retry on their own
// (docs/adr/0005-temporal-for-background-work.md), and the client retries
// anyway: its retries absorb a short rate limit inside one activity attempt,
// so a single 429 does not make the whole activity, and every Chunk it had
// already embedded, run again.
package embedding

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/phongnd2802/trelis/internal/config"
	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/models/api"
)

// defaultPath is where an OpenAI-compatible provider takes embeddings, under
// the role's base URL.
const defaultPath = "/embeddings"

// Embedder is the embedding model role.
type Embedder interface {
	// Embed returns one vector per text, in the order of texts.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// GetModelName returns the model name sent on the wire.
	GetModelName() string
}

// Option configures the client New builds.
type Option func(*client)

// WithHTTPClient sends requests through hc instead of the shared client in
// internal/models/api.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *client) { c.endpoint.Client = hc }
}

// WithRetry bounds the retries of one batch: maxAttempts sends in total (a
// value below 1 sends the batch once), waiting baseDelay before the second
// and doubling that wait up to maxDelay. Negative waits mean none.
func WithRetry(maxAttempts int, baseDelay, maxDelay time.Duration) Option {
	return func(c *client) {
		c.retry.MaxAttempts = maxAttempts
		c.retry.BaseDelay = baseDelay
		c.retry.MaxDelay = maxDelay
	}
}

// WithMaxRetryAfter bounds how long a Retry-After is waited out. A provider
// that asks for longer than d gets its error back instead of a client that
// either stalls or comes back earlier than it was told to. Zero or less waits
// out none: any positive Retry-After returns the error at once.
func WithMaxRetryAfter(d time.Duration) Option {
	return func(c *client) { c.retry.MaxRetryAfter = d }
}

// client is the embedding role's client.
type client struct {
	endpoint  api.Endpoint
	batchSize int
	// retry is how one batch is sent again after a failure worth waiting out.
	retry api.Retry
	// timeout is the fallback deadline of ONE request, applied only when the
	// caller's ctx carries none. It is per request rather than per Embed
	// because a long ingest sends dozens of batches, and one deadline over all
	// of them would cut the call off and throw away the batches that finished.
	timeout time.Duration
}

// New builds the client for one configured embedding role (models.embedding).
func New(role config.EmbeddingRole, opts ...Option) (Embedder, error) {
	endpoint, err := role.APIEndpoint("embedding")
	if err != nil {
		return nil, err
	}
	c := &client{
		endpoint:  endpoint,
		batchSize: role.BatchSize,
		retry:     api.DefaultRetry(),
		// Embeddings are non-streaming calls, so the chat timeout is the right
		// fallback for one of them.
		timeout: api.DefaultChatTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	// A role built in code rather than loaded from config carries no batch
	// size; it batches like the configured default does.
	if c.batchSize <= 0 {
		c.batchSize = config.DefaultEmbeddingBatchSize
	}
	return c, nil
}

// GetModelName returns the wire model name.
func (c *client) GetModelName() string { return c.endpoint.Model }

// Embed turns texts into vectors, one per text and in the order of texts. The
// caller's ctx bounds the whole call; a request whose ctx carries no deadline
// gets c.timeout of its own.
func (c *client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	vectors := make([][]float32, 0, len(texts))
	// The vector length the batches before this one came back in, or 0 when
	// none has yet.
	dims := 0
	// One batch at a time: sending them in parallel would ignore the very limit
	// a rate-limited provider is reporting.
	for start := 0; start < len(texts); start += c.batchSize {
		batch := texts[start:min(start+c.batchSize, len(texts))]
		got, err := c.embedBatch(ctx, batch, dims)
		if err != nil {
			return nil, fmt.Errorf("embed texts %d-%d: %w", start+1, start+len(batch), err)
		}
		// One Embed call feeds one index, so every batch has to come back in
		// the same shape: a distance between two vectors of different lengths
		// would mean nothing. got has a first element because the batch is
		// non-empty, vectorsOf checked the count and rejected empty vectors.
		if dims == 0 {
			dims = len(got[0])
		}
		vectors = append(vectors, got...)
	}
	return vectors, nil
}

// embedBatch sends one batch and returns its vectors, sending it again while
// the provider reports a failure worth waiting out. The error is the last one
// the provider gave. dims is the vector length an earlier batch of the same
// Embed call established, or 0 when this is the first.
func (c *client) embedBatch(ctx context.Context, batch []string, dims int) ([][]float32, error) {
	start := time.Now()
	var (
		vectors [][]float32
		used    *usage
	)
	attempts, err := c.retry.DoWithAttempts(ctx, func(ctx context.Context) error {
		got, gotUsage, err := c.send(ctx, batch, dims)
		if err != nil {
			return err
		}
		// Keep only the successful attempt's accounting: a retried attempt's
		// usage was not billed as part of this line.
		vectors, used = got, gotUsage
		return nil
	})
	call := logger.ModelCall{
		Role:       "embedding",
		Model:      c.endpoint.Model,
		Protocol:   "embeddings",
		Duration:   time.Since(start),
		Attempts:   attempts,
		Failed:     err != nil,
		HTTPStatus: api.HTTPStatusCode(err),
	}
	if used != nil {
		call.HasUsage = true
		call.InputTokens = used.PromptTokens
		if call.InputTokens == 0 {
			call.InputTokens = used.TotalTokens
		}
	}
	logger.LogModelCall(ctx, call)
	if err != nil {
		return nil, err
	}
	return vectors, nil
}

// send posts one batch and returns its vectors in the batch's order and the
// vendor's token accounting, when the reply carries it. dims is the vector
// length an earlier batch of the same Embed call established, or 0 to accept
// whatever this reply carries.
func (c *client) send(ctx context.Context, batch []string, dims int) ([][]float32, *usage, error) {
	body := requestBody{Model: c.endpoint.Model, Input: batch}
	raw, err := c.endpoint.PostJSON(ctx, defaultPath, body, c.timeout)
	if err != nil {
		return nil, nil, err
	}
	return vectorsOf(raw, len(batch), dims)
}

// requestBody is the OpenAI embeddings request. The vectors come back as float
// arrays, so no encoding_format is sent, and `dimensions` is left out because
// the role's model is what decides how long a vector is.
type requestBody struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// usage is the vendor's token accounting for one embeddings request. A nil
// *usage means the reply carried none.
type usage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// responseBody is the OpenAI embeddings reply.
type responseBody struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage *usage `json:"usage"`
	// Error carries the vendor's message when a gateway answers 200 with an
	// error instead of a status code.
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// vectorsOf reads a reply to a batch of want texts and puts its vectors in the
// batch's order: a provider numbers each vector with the index of the text it
// belongs to and may answer in any order. dims is the vector length the caller
// expects, or 0 to adopt whatever this reply carries. The usage pointer is nil
// when the reply carries no usage object.
func vectorsOf(raw []byte, want, dims int) ([][]float32, *usage, error) {
	var body responseBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, nil, fmt.Errorf("decode response: %w", err)
	}
	if body.Error != nil && body.Error.Message != "" {
		return nil, nil, fmt.Errorf("API error: %s", body.Error.Message)
	}
	// A reply that does not carry one vector per text is not an answer to this
	// batch. It would come back the same way every time, so sending the batch
	// again cannot help.
	if len(body.Data) != want {
		return nil, nil, fmt.Errorf("response carries %d vectors, want %d", len(body.Data), want)
	}
	vectors := make([][]float32, want)
	seen := make([]bool, want)
	for _, data := range body.Data {
		if data.Index < 0 || data.Index >= want || seen[data.Index] {
			return nil, nil, fmt.Errorf("response index %d is not one of 0..%d, or comes twice", data.Index, want-1)
		}
		seen[data.Index] = true
		vectors[data.Index] = data.Embedding
	}
	// An empty vector is not an answer, and vectors of two lengths cannot share
	// one index: a distance between them would mean nothing. Like the checks
	// above this is the provider's own reply, so it is a plain error that
	// retryable leaves alone. The index named is the text's place in the batch.
	for i, v := range vectors {
		if len(v) == 0 {
			return nil, nil, fmt.Errorf("vector %d is empty", i)
		}
		if dims == 0 {
			dims = len(v)
			continue
		}
		if len(v) != dims {
			return nil, nil, fmt.Errorf("vector %d has %d dimensions, want %d", i, len(v), dims)
		}
	}
	return vectors, body.Usage, nil
}
