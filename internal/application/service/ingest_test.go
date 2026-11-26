package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/types"
)

// ingestDocument creates one pending Document owned by ownerID.
func ingestDocument(t *testing.T, store db.Store, ownerID uuid.UUID) *types.Document {
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
	if err := repository.NewDocumentRepository(store).CreateDocument(context.Background(), doc, types.AccessGrants{}); err != nil {
		t.Fatalf("create document: %v", err)
	}
	return doc
}

func TestCompleteRefusesWhileAChunkHasNoEmbedding(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	owner, err := store.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	doc := ingestDocument(t, store, owner.ID)
	documents := repository.NewDocumentRepository(store)
	chunks := repository.NewChunkRepository(store)
	if err := store.ExecTx(ctx, func(ctx context.Context) error {
		return chunks.ReplaceChunks(ctx, doc.ID, []*types.Chunk{
			{Seq: 0, Content: "Điều 1.", StartOffset: 0, EndOffset: 7},
			{Seq: 1, Content: "Điều 2.", StartOffset: 8, EndOffset: 15},
		})
	}); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}
	unembedded, err := chunks.ListUnembeddedChunks(ctx, doc.ID, 0, 2)
	if err != nil || len(unembedded) != 2 {
		t.Fatalf("ListUnembeddedChunks = %+v, %v; want both Chunks", unembedded, err)
	}
	if err := chunks.SetChunkEmbedding(ctx, unembedded[0].ID, []float32{1, 2}, "test-embedding"); err != nil {
		t.Fatalf("SetChunkEmbedding: %v", err)
	}

	// Complete needs only the repositories; the parser, file service and
	// embedder play no part in it.
	ingest := NewIngestService(store, documents, chunks, nil, nil, nil)
	err = ingest.Complete(ctx, doc.ID)
	if err == nil || !strings.Contains(err.Error(), "1") || !strings.Contains(err.Error(), "without an embedding") {
		t.Fatalf("Complete with one unembedded Chunk = %v, want an error naming the missing count", err)
	}
	got, err := documents.GetDocumentByID(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParseStatus == types.ParseStatusCompleted {
		t.Error("Document is completed with a Chunk that has no embedding")
	}

	if err := chunks.SetChunkEmbedding(ctx, unembedded[1].ID, []float32{3, 4}, "test-embedding"); err != nil {
		t.Fatalf("SetChunkEmbedding: %v", err)
	}
	if err := ingest.Complete(ctx, doc.ID); err != nil {
		t.Fatalf("Complete after every Chunk was embedded: %v", err)
	}
	got, err = documents.GetDocumentByID(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParseStatus != types.ParseStatusCompleted {
		t.Errorf("parse_status = %q, want completed", got.ParseStatus)
	}

	var missing *types.DocumentNotFoundError
	if err := ingest.Complete(ctx, uuid.New()); !errors.As(err, &missing) {
		t.Errorf("Complete(unknown Document) = %v, want *types.DocumentNotFoundError", err)
	}
	// There is no row left to mark when the Document is gone, which is not an
	// error for the workflow that is already ending.
	if err := ingest.MarkFailed(ctx, uuid.New(), "the Source File could not be parsed"); err != nil {
		t.Errorf("MarkFailed(unknown Document) = %v, want nil", err)
	}
}
