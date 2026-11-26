package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/types"
)

// document creates one pending Document owned by alice.
func document(t *testing.T, s db.Store, ownerID uuid.UUID) *types.Document {
	t.Helper()
	doc := &types.Document{
		ID:        uuid.New(),
		OwnerID:   ownerID,
		Title:     "luat-thue.txt",
		FileName:  "luat-thue.txt",
		FileType:  "txt",
		FileSize:  1,
		FileHash:  uuid.NewString(),
		UploadID:  uuid.New(),
		ObjectKey: "documents/" + uuid.NewString() + "/source",
	}
	if err := NewDocumentRepository(s).CreateDocument(context.Background(), doc, types.AccessGrants{}); err != nil {
		t.Fatalf("create document: %v", err)
	}
	return doc
}

func TestReplaceChunksLeavesTheSameSetOnEveryRun(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	doc := document(t, s, ids["alice"])
	repo := NewChunkRepository(s)

	first := []*types.Chunk{
		{Seq: 0, Content: "Điều 1.", StartOffset: 0, EndOffset: 7},
		{Seq: 1, Content: "Điều 2.", ContextHeader: "Chương I", StartOffset: 8, EndOffset: 15},
	}
	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return repo.ReplaceChunks(ctx, doc.ID, first)
	}); err != nil {
		t.Fatalf("first ReplaceChunks: %v", err)
	}
	// Running the same input again, as a retried activity would, must not
	// duplicate the rows.
	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return repo.ReplaceChunks(ctx, doc.ID, first)
	}); err != nil {
		t.Fatalf("second ReplaceChunks: %v", err)
	}
	got, err := repo.ListUnembeddedChunks(ctx, doc.ID, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 0 || got[0].Content != "Điều 1." ||
		got[1].Seq != 1 || got[1].ContextHeader != "Chương I" ||
		got[0].StartOffset != 0 || got[0].EndOffset != 7 || got[1].StartOffset != 8 || got[1].EndOffset != 15 {
		t.Errorf("chunks after two identical runs = %+v, want the two inserted once", got)
	}

	// A new set replaces the old one rather than appending to it.
	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return repo.ReplaceChunks(ctx, doc.ID, []*types.Chunk{{Seq: 0, Content: "Điều 3.", StartOffset: 16, EndOffset: 23}})
	}); err != nil {
		t.Fatalf("third ReplaceChunks: %v", err)
	}
	got, err = repo.ListUnembeddedChunks(ctx, doc.ID, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "Điều 3." {
		t.Errorf("chunks after a replacement = %+v, want only the new Chunk", got)
	}
}

func TestSetChunkEmbeddingStoresTheVectorAndModel(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	s := db.NewStore(pool)
	owner, err := s.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	doc := document(t, s, owner.ID)
	repo := NewChunkRepository(s)
	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return repo.ReplaceChunks(ctx, doc.ID, []*types.Chunk{
			{Seq: 0, Content: "Điều 1.", StartOffset: 0, EndOffset: 7},
			{Seq: 1, Content: "Điều 2.", ContextHeader: "Chương I", StartOffset: 8, EndOffset: 15},
		})
	}); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}

	chunks, err := repo.ListUnembeddedChunks(ctx, doc.ID, 0, 2)
	if err != nil {
		t.Fatalf("ListUnembeddedChunks: %v", err)
	}
	if len(chunks) != 2 || chunks[0].ID == uuid.Nil || chunks[0].Seq != 0 || chunks[0].Content != "Điều 1." ||
		chunks[1].ContextHeader != "Chương I" || chunks[1].StartOffset != 8 || chunks[1].EndOffset != 15 {
		t.Fatalf("ListUnembeddedChunks = %+v, want the two Chunks in seq order", chunks)
	}
	if count, err := repo.CountChunksWithoutEmbedding(ctx, doc.ID); err != nil || count != 2 {
		t.Fatalf("CountChunksWithoutEmbedding = %d, %v; want 2", count, err)
	}

	if err := repo.SetChunkEmbedding(ctx, chunks[0].ID, []float32{0.5, -1.25, 3}, "test-embedding"); err != nil {
		t.Fatalf("SetChunkEmbedding: %v", err)
	}
	var vector, model string
	if err := pool.QueryRow(ctx, "SELECT embedding::text, embedding_model FROM chunks WHERE id = $1", chunks[0].ID).
		Scan(&vector, &model); err != nil {
		t.Fatal(err)
	}
	if vector != "[0.5,-1.25,3]" || model != "test-embedding" {
		t.Errorf("stored embedding = %q by %q, want [0.5,-1.25,3] by test-embedding", vector, model)
	}
	var embedded bool
	if err := pool.QueryRow(ctx, "SELECT embedding IS NOT NULL FROM chunks WHERE id = $1", chunks[1].ID).Scan(&embedded); err != nil {
		t.Fatal(err)
	}
	if embedded {
		t.Error("the second Chunk has an embedding, want it left NULL")
	}

	// An already embedded range is a no-op, and the count sees the rest.
	if got, err := repo.ListUnembeddedChunks(ctx, doc.ID, 0, 1); err != nil || len(got) != 0 {
		t.Errorf("ListUnembeddedChunks(0, 1) = %+v, %v; want nothing already embedded", got, err)
	}
	remaining, err := repo.ListUnembeddedChunks(ctx, doc.ID, 0, 2)
	if err != nil || len(remaining) != 1 || remaining[0].ID != chunks[1].ID {
		t.Fatalf("ListUnembeddedChunks after embedding one = %+v, %v; want only the second Chunk", remaining, err)
	}
	if count, err := repo.CountChunksWithoutEmbedding(ctx, doc.ID); err != nil || count != 1 {
		t.Errorf("CountChunksWithoutEmbedding = %d, %v; want 1", count, err)
	}

	// A Chunk that disappeared between listing and updating is reported, not
	// skipped: the activity then retries against the set it has now.
	if err := repo.SetChunkEmbedding(ctx, uuid.New(), []float32{1}, "test-embedding"); err == nil {
		t.Error("SetChunkEmbedding(unknown chunk) = nil, want an error")
	}
}

func TestSetParseStatusKeepsAFailureReasonOnlyWhileFailed(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	doc := document(t, s, ids["alice"])
	repo := NewDocumentRepository(s)

	if err := repo.SetParseStatus(ctx, doc.ID, types.ParseStatusProcessing, ""); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetDocumentByID(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParseStatus != types.ParseStatusProcessing || got.ParseError != "" {
		t.Errorf("processing Document = %q/%q, want processing with no error", got.ParseStatus, got.ParseError)
	}

	if err := repo.SetParseStatus(ctx, doc.ID, types.ParseStatusFailed, "the Source File could not be parsed"); err != nil {
		t.Fatal(err)
	}
	got, err = repo.GetDocumentByID(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParseStatus != types.ParseStatusFailed || got.ParseError != "the Source File could not be parsed" {
		t.Errorf("failed Document = %q/%q, want failed with its reason", got.ParseStatus, got.ParseError)
	}

	if err := repo.SetParseStatus(ctx, doc.ID, types.ParseStatusCompleted, ""); err != nil {
		t.Fatal(err)
	}
	got, err = repo.GetDocumentByID(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParseStatus != types.ParseStatusCompleted || got.ParseError != "" {
		t.Errorf("completed Document = %q/%q, want completed with the reason cleared", got.ParseStatus, got.ParseError)
	}

	if _, err := repo.GetDocumentByID(ctx, uuid.New()); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("GetDocumentByID(unknown) = %v, want ErrDocumentNotFound", err)
	}
	if err := repo.SetParseStatus(ctx, uuid.New(), types.ParseStatusCompleted, ""); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("SetParseStatus(unknown) = %v, want ErrDocumentNotFound", err)
	}
}
