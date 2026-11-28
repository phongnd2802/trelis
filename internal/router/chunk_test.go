package router

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/application/service"
	"github.com/phongnd2802/trelis/internal/handler"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// readableChunkReader answers each Principal only the Chunks installed for
// them, so a test can give two callers different access.
type readableChunkReader struct {
	interfaces.ChunkReader
	byPrincipal map[uuid.UUID]map[uuid.UUID]*types.Chunk
}

func (f *readableChunkReader) GetReadableChunk(_ context.Context, principalID, id uuid.UUID) (*types.Chunk, error) {
	if chunk, ok := f.byPrincipal[principalID][id]; ok {
		return chunk, nil
	}
	return nil, repository.ErrChunkNotFound
}

// newChunkRouter is the router on one ChunkService, with the other routes
// unmounted because these tests never call them.
func newChunkRouter(reader interfaces.ChunkReader) http.Handler {
	bob := types.Principal{ID: uuid.MustParse("0190d2a8-0000-7000-8000-000000000002"), Name: "bob"}
	me := handler.NewPrincipalHandler(service.NewPrincipalService(nil, nil, nil))
	return New(Readiness{}, keyring{"trelis_alice": alice, "trelis_bob": bob}, me, nil, nil, nil, nil,
		handler.NewChunkHandler(service.NewChunkService(reader)), false)
}

func TestChunkNeedsAKey(t *testing.T) {
	h := newChunkRouter(&readableChunkReader{})
	rec := get(t, h, "/chunks/0190d2a8-0000-7000-8000-000000000001", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /chunks/{id} without a key = %d, want 401", rec.Code)
	}
}

func TestGetChunkAnswersTheChunkWithoutAnEmbedding(t *testing.T) {
	chunk := &types.Chunk{
		ID:            uuid.MustParse("0190d2a8-0000-7000-8000-000000000001"),
		DocumentID:    uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1"),
		Seq:           2,
		Content:       "Điều 1. Phạm vi điều chỉnh.",
		ContextHeader: "Chương I",
		StartOffset:   10,
		EndOffset:     40,
	}
	reader := &readableChunkReader{byPrincipal: map[uuid.UUID]map[uuid.UUID]*types.Chunk{
		alice.ID: {chunk.ID: chunk},
	}}
	h := newChunkRouter(reader)

	rec := get(t, h, "/chunks/"+chunk.ID.String(), "Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /chunks/%s = %d %s, want 200", chunk.ID, rec.Code, rec.Body)
	}
	var got types.Chunk
	if err := json.Unmarshal(decode(t, rec).Data, &got); err != nil {
		t.Fatalf("data in %s is not a Chunk: %v", rec.Body, err)
	}
	if got.ID != chunk.ID || got.DocumentID != chunk.DocumentID || got.Seq != chunk.Seq ||
		got.Content != chunk.Content || got.ContextHeader != chunk.ContextHeader ||
		got.StartOffset != chunk.StartOffset || got.EndOffset != chunk.EndOffset {
		t.Errorf("chunk = %+v, want the stored Chunk %+v", got, chunk)
	}
	if strings.Contains(rec.Body.String(), "embedding") {
		t.Errorf("body = %s, want no embedding field", rec.Body)
	}
}

func TestAnUnreadableChunkAnswersTheSame404AsAnUnknownOne(t *testing.T) {
	chunk := &types.Chunk{
		ID:         uuid.MustParse("0190d2a8-0000-7000-8000-000000000001"),
		DocumentID: uuid.MustParse("0190d2a8-0000-7000-8000-0000000000b1"),
		Content:    "alice secret",
	}
	reader := &readableChunkReader{byPrincipal: map[uuid.UUID]map[uuid.UUID]*types.Chunk{
		alice.ID: {chunk.ID: chunk},
	}}
	h := newChunkRouter(reader)

	unreadable := get(t, h, "/chunks/"+chunk.ID.String(), "Bearer trelis_bob")
	unknown := get(t, h, "/chunks/"+uuid.NewString(), "Bearer trelis_bob")
	for name, rec := range map[string]int{"unreadable": unreadable.Code, "unknown": unknown.Code} {
		if rec != http.StatusNotFound {
			t.Errorf("%s Chunk = %d, want 404", name, rec)
		}
	}
	if unreadable.Body.String() != unknown.Body.String() {
		t.Errorf("unreadable body = %s, unknown body = %s; want the same body so existence is not revealed",
			unreadable.Body, unknown.Body)
	}

	if rec := get(t, h, "/chunks/"+chunk.ID.String(), "Bearer trelis_alice"); rec.Code != http.StatusOK {
		t.Errorf("alice GET her own Chunk = %d, want 200", rec.Code)
	}
}

func TestANonUUIDChunkIDIs400(t *testing.T) {
	h := newChunkRouter(&readableChunkReader{})
	rec := get(t, h, "/chunks/not-a-uuid", "Bearer trelis_alice")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET /chunks/not-a-uuid = %d %s, want 400", rec.Code, rec.Body)
	}
	e := decode(t, rec)
	if e.Success || e.Error == nil || e.Error.Code != 1000 {
		t.Errorf("body = %s, want the bad-request error envelope", rec.Body)
	}
}
