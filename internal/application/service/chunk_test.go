package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// fakeChunkReader answers from a fixed set of readable Chunks and reports
// every other ID as not found, the way the readable-Chunk query does.
type fakeChunkReader struct {
	interfaces.ChunkReader
	chunks       map[uuid.UUID]*types.Chunk
	gotPrincipal uuid.UUID
	gotID        uuid.UUID
	calls        int
}

func (f *fakeChunkReader) GetReadableChunk(_ context.Context, principalID, id uuid.UUID) (*types.Chunk, error) {
	f.calls++
	f.gotPrincipal, f.gotID = principalID, id
	if chunk, ok := f.chunks[id]; ok {
		return chunk, nil
	}
	return nil, repository.ErrChunkNotFound
}

func TestGetChunkByIDReturnsAReadableChunk(t *testing.T) {
	chunk := &types.Chunk{
		ID:            uuid.MustParse("0190d2a8-0000-7000-8000-000000000001"),
		DocumentID:    uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1"),
		Seq:           2,
		Content:       "Điều 1.",
		ContextHeader: "Chương I",
	}
	reader := &fakeChunkReader{chunks: map[uuid.UUID]*types.Chunk{chunk.ID: chunk}}
	principalID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a1")

	got, err := NewChunkService(reader).GetChunkByID(callerContext(principalID), chunk.ID)
	if err != nil {
		t.Fatalf("GetChunkByID: %v", err)
	}
	if got != chunk {
		t.Errorf("chunk = %+v, want the readable Chunk", got)
	}
	if reader.gotPrincipal != principalID || reader.gotID != chunk.ID {
		t.Errorf("reader got principal %s and chunk %s, want %s and %s",
			reader.gotPrincipal, reader.gotID, principalID, chunk.ID)
	}
}

func TestAnUnknownOrUnreadableChunkIsTheSameNotFoundError(t *testing.T) {
	chunk := &types.Chunk{
		ID:         uuid.MustParse("0190d2a8-0000-7000-8000-000000000001"),
		DocumentID: uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1"),
		Content:    "secret",
	}
	reader := &fakeChunkReader{chunks: map[uuid.UUID]*types.Chunk{}}
	svc := NewChunkService(reader)

	_, err := svc.GetChunkByID(callerContext(uuid.New()), chunk.ID)
	appErr, ok := apperrors.IsAppError(err)
	if !ok || appErr.HTTPCode != 404 || appErr.Code != apperrors.ErrNotFound || !strings.Contains(appErr.Message, "Chunk") {
		t.Errorf("GetChunkByID(unreadable) = %v, want a 404 Chunk-not-found error", err)
	}
}

// TestTheChunkReadNeedsACaller guards the assumption every route behind the
// authentication middleware relies on.
func TestTheChunkReadNeedsACaller(t *testing.T) {
	reader := &fakeChunkReader{chunks: map[uuid.UUID]*types.Chunk{}}
	if _, err := NewChunkService(reader).GetChunkByID(context.Background(), uuid.New()); err == nil {
		t.Error("GetChunkByID without a Principal = nil, want an error")
	}
	if reader.calls != 0 {
		t.Errorf("reader called %d times without a Principal, want none", reader.calls)
	}
}

var _ interfaces.ChunkService = NewChunkService(nil)
