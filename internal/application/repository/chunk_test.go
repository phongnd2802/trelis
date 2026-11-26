package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/db"
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
	got, err := s.Querier(ctx).ListChunksByDocument(ctx, doc.ID)
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
	got, err = s.Querier(ctx).ListChunksByDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "Điều 3." {
		t.Errorf("chunks after a replacement = %+v, want only the new Chunk", got)
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
}
