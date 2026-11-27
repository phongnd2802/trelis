package interfaces

import (
	"context"

	"github.com/phongnd2802/trelis/internal/types"
)

// RetrievalService answers a query with ranked Chunks, always only from the
// Documents the Principal in ctx can read. Dense is the only Retrieval
// Technique this build implements; Retrieve validates the config against
// that and rejects any other name. It answers with *errors.AppError for
// anything the client should see.
type RetrievalService interface {
	// Retrieve validates req, resolves the caller's readable Documents, runs
	// the dense search over them and returns its ranked Chunks with the trace
	// of the steps it took.
	Retrieve(ctx context.Context, req *types.RetrievalRequest) (*types.RetrievalResult, error)
}
