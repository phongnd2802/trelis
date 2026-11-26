package interfaces

import "context"

// Embedder is the embedding model role the ingest service calls. The model
// client in internal/models/embedding satisfies it; declaring it here keeps
// the application layer from depending on that package.
type Embedder interface {
	// Embed returns one vector per text, in the order of texts.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// GetModelName returns the model name stored beside each vector.
	GetModelName() string
}
