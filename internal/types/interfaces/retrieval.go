package interfaces

import (
	"context"

	"github.com/phongnd2802/trelis/internal/types"
)

// RetrievalService answers a query with ranked Chunks, always only from the
// Documents the Principal in ctx can read. Retrieve validates the config
// against the Retrieval Techniques this build implements and runs every
// candidate one it names, fusing several with Reciprocal Rank Fusion and
// reranking when the config asks for it; it answers with *errors.AppError for
// anything the client should see.
type RetrievalService interface {
	// Retrieve validates req, resolves the caller's readable Documents, runs
	// the requested candidate Retrieval Techniques over them and returns their
	// ranked Chunks, fused when more than one ran and reranked when requested,
	// with the trace of the steps it took.
	Retrieve(ctx context.Context, req *types.RetrievalRequest) (*types.RetrievalResult, error)
}
