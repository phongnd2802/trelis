package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	werrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// chunkService implements the Chunk read use case over the readable-Chunk
// query.
type chunkService struct {
	chunks interfaces.ChunkReader
}

// NewChunkService returns the Chunk read use case.
func NewChunkService(chunks interfaces.ChunkReader) interfaces.ChunkService {
	return &chunkService{chunks: chunks}
}

// GetChunkByID returns a Chunk the caller can read. A Chunk that does not
// exist and one whose Document the caller cannot read are the same not-found
// error, so the answer reveals nothing about what exists (ADR-0004).
func (s *chunkService) GetChunkByID(ctx context.Context, id uuid.UUID) (*types.Chunk, error) {
	p, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	chunk, err := s.chunks.GetReadableChunk(ctx, p.ID, id)
	if errors.Is(err, repository.ErrChunkNotFound) {
		return nil, werrors.NewNotFoundError("Chunk not found")
	}
	if err != nil {
		return nil, err
	}
	return chunk, nil
}
