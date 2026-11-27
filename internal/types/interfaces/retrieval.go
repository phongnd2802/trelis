package interfaces

import (
	"context"

	"github.com/phongnd2802/trelis/internal/types"
)

// RetrievalService answers a query with ranked Chunks, always only from the
// Documents the Principal in ctx can read. Retrieve validates the config
// against the Retrieval Techniques this build implements and runs the one it
// names; it answers with *errors.AppError for anything the client should see.
type RetrievalService interface {
	// Retrieve validates req, resolves the caller's readable Documents, runs
	// the requested Retrieval Technique over them and returns its ranked
	// Chunks with the trace of the steps it took.
	Retrieve(ctx context.Context, req *types.RetrievalRequest) (*types.RetrievalResult, error)
}
