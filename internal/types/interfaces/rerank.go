package interfaces

import "context"

// Reranker is the rerank model role retrieval calls. The model client in
// internal/models/rerank satisfies it; declaring it here keeps the
// application layer from depending on that package.
type Reranker interface {
	// Rerank scores each Chunk text against query, one score per text and in
	// the order of texts.
	Rerank(ctx context.Context, query string, texts []string) ([]float64, error)

	// GetModelName returns the model name sent on the wire.
	GetModelName() string
}
