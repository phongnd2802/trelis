package repository

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/types"
)

// sortedIDs returns ids in UUID-string order, the order
// ListReadableDocumentIDs promises.
func sortedIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func TestListReadableDocumentIDsReturnsOnlyCompletedReadableDocuments(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	docs := NewDocumentRepository(s)

	ownCompleted := document(t, s, ids["alice"])
	ownProcessing := document(t, s, ids["alice"])
	ownFailed := document(t, s, ids["alice"])
	sharedWithAlice := documentGranted(t, s, ids["bob"], types.AccessGrants{PrincipalIDs: []uuid.UUID{ids["alice"]}})
	bobPrivate := document(t, s, ids["bob"])

	for doc, status := range map[*types.Document]string{
		ownCompleted:    types.ParseStatusCompleted,
		ownProcessing:   types.ParseStatusProcessing,
		ownFailed:       types.ParseStatusFailed,
		sharedWithAlice: types.ParseStatusCompleted,
		bobPrivate:      types.ParseStatusCompleted,
	} {
		if err := docs.SetParseStatus(ctx, doc.ID, status, ""); err != nil {
			t.Fatalf("SetParseStatus(%s): %v", status, err)
		}
	}

	got, err := docs.ListReadableDocumentIDs(ctx, ids["alice"])
	if err != nil {
		t.Fatalf("ListReadableDocumentIDs(alice): %v", err)
	}
	want := sortedIDs([]uuid.UUID{ownCompleted.ID, sharedWithAlice.ID})
	if !slices.Equal(got, want) {
		t.Errorf("alice's IDs = %v, want %v (completed and readable only)", got, want)
	}

	got, err = docs.ListReadableDocumentIDs(ctx, ids["bob"])
	if err != nil {
		t.Fatalf("ListReadableDocumentIDs(bob): %v", err)
	}
	want = sortedIDs([]uuid.UUID{sharedWithAlice.ID, bobPrivate.ID})
	if !slices.Equal(got, want) {
		t.Errorf("bob's IDs = %v, want %v", got, want)
	}
}

func TestListPendingDocumentsReturnsOnlyPendingDocumentsWithTheirUpload(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	docs := NewDocumentRepository(s)

	pending := document(t, s, ids["alice"])
	for _, status := range []string{types.ParseStatusProcessing, types.ParseStatusCompleted, types.ParseStatusFailed} {
		doc := document(t, s, ids["alice"])
		if err := docs.SetParseStatus(ctx, doc.ID, status, ""); err != nil {
			t.Fatalf("SetParseStatus(%s): %v", status, err)
		}
	}

	got, err := docs.ListPendingDocuments(ctx)
	if err != nil {
		t.Fatalf("ListPendingDocuments: %v", err)
	}
	want := []types.PendingDocument{{
		DocumentID: pending.ID,
		UploadID:   pending.UploadID,
	}}
	if !slices.Equal(got, want) {
		t.Errorf("pending documents = %+v, want %+v", got, want)
	}
}

func TestListDocumentUploadsReturnsTheCurrentUploadAndStatusOfEachDocument(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	docs := NewDocumentRepository(s)
	aliceDoc, bobDoc := document(t, s, ids["alice"]), document(t, s, ids["bob"])
	if err := docs.SetParseStatus(ctx, bobDoc.ID, types.ParseStatusCompleted, ""); err != nil {
		t.Fatalf("SetParseStatus: %v", err)
	}

	got, err := docs.ListDocumentUploads(ctx, []uuid.UUID{aliceDoc.ID, bobDoc.ID, uuid.New()})
	if err != nil {
		t.Fatalf("ListDocumentUploads: %v", err)
	}
	want := map[uuid.UUID]types.DocumentUpload{
		aliceDoc.ID: {
			DocumentID:  aliceDoc.ID,
			UploadID:    aliceDoc.UploadID,
			ParseStatus: types.ParseStatusPending,
		},
		bobDoc.ID: {
			DocumentID:  bobDoc.ID,
			UploadID:    bobDoc.UploadID,
			ParseStatus: types.ParseStatusCompleted,
		},
	}
	if !maps.Equal(got, want) {
		t.Errorf("uploads = %+v, want %+v", got, want)
	}

	got, err = docs.ListDocumentUploads(ctx, nil)
	if err != nil || len(got) != 0 {
		t.Errorf("ListDocumentUploads(nil) = %v, %v; want an empty map and no error", got, err)
	}
}

func TestReplaceDocumentUploadAppliesOnlyWhileThePreviousUploadIsCurrent(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	docs := NewDocumentRepository(s)
	doc := document(t, s, ids["alice"])

	updated := *doc
	updated.UploadID = uuid.New()
	updated.Title = "v2.txt"
	updated.FileName = "v2.txt"
	updated.FileType = "txt"
	updated.FileSize = 2
	updated.FileHash = uuid.NewString()
	updated.ObjectKey = "documents/" + doc.ID.String() + "/" + updated.UploadID.String() + "/source"

	got, err := docs.ReplaceDocumentUpload(ctx, &updated, doc.UploadID)
	if err != nil {
		t.Fatalf("ReplaceDocumentUpload: %v", err)
	}
	if got.UploadID != updated.UploadID || got.FileName != "v2.txt" || got.FileHash != updated.FileHash ||
		got.ParseStatus != types.ParseStatusPending {
		t.Errorf("replaced Document = %+v, want the new upload, file metadata and pending status", got)
	}

	// The old upload ID is no longer current, so a second replacement of the
	// version the caller read finds no row.
	stale := updated
	stale.UploadID = uuid.New()
	if _, err := docs.ReplaceDocumentUpload(ctx, &stale, doc.UploadID); !errors.Is(err, ErrUploadChanged) {
		t.Errorf("ReplaceDocumentUpload with a stale previous upload = %v, want ErrUploadChanged", err)
	}
	if _, err := docs.ReplaceDocumentUpload(ctx, &stale, uuid.New()); !errors.Is(err, ErrUploadChanged) {
		t.Errorf("ReplaceDocumentUpload with an unknown previous upload = %v, want ErrUploadChanged", err)
	}

	if _, err := docs.DeleteDocument(ctx, doc.ID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if _, err := docs.ReplaceDocumentUpload(ctx, &stale, doc.UploadID); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("ReplaceDocumentUpload of a deleted Document = %v, want ErrDocumentNotFound", err)
	}
}

func TestGetAndLockDocumentUploadID(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	docs := NewDocumentRepository(s)
	doc := document(t, s, ids["alice"])

	got, err := docs.GetDocumentUploadID(ctx, doc.ID)
	if err != nil || got != doc.UploadID {
		t.Errorf("GetDocumentUploadID = %s, %v; want %s", got, err, doc.UploadID)
	}
	got, err = docs.LockDocumentUploadID(ctx, doc.ID)
	if err != nil || got != doc.UploadID {
		t.Errorf("LockDocumentUploadID = %s, %v; want %s", got, err, doc.UploadID)
	}
	if _, err := docs.GetDocumentUploadID(ctx, uuid.New()); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("GetDocumentUploadID(unknown) = %v, want ErrDocumentNotFound", err)
	}
	if _, err := docs.LockDocumentUploadID(ctx, uuid.New()); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("LockDocumentUploadID(unknown) = %v, want ErrDocumentNotFound", err)
	}
}

func TestDeleteDocumentRemovesItsChunksParentChunksAndAccessList(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	s := db.NewStore(pool)
	owner, err := s.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := s.Querier(ctx).CreatePrincipal(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.Querier(ctx).CreateGroup(ctx, "legal")
	if err != nil {
		t.Fatal(err)
	}
	doc := documentGranted(t, s, owner.ID, types.AccessGrants{
		PrincipalIDs: []uuid.UUID{reader.ID},
		GroupIDs:     []uuid.UUID{group.ID},
	})
	set := types.ChunkSet{
		Parents: []*types.ParentChunk{
			{Seq: 0, Content: "Điều 1. Phạm vi\nĐiều 2. Đối tượng", StartOffset: 0, EndOffset: 35},
		},
		Chunks: []*types.Chunk{
			{Seq: 0, Content: "Điều 1. Phạm vi", StartOffset: 0, EndOffset: 16},
			{Seq: 1, Content: "Điều 2. Đối tượng", StartOffset: 17, EndOffset: 35},
		},
		ParentOf: []int{0, 0},
	}
	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return NewChunkRepository(s).ReplaceChunks(ctx, doc.ID, set)
	}); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}

	docs := NewDocumentRepository(s)
	deletedUpload, err := docs.DeleteDocument(ctx, doc.ID)
	if err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if deletedUpload != doc.UploadID {
		t.Errorf("DeleteDocument returned upload %s, want the row's %s", deletedUpload, doc.UploadID)
	}

	if _, err := docs.GetDocumentByID(ctx, doc.ID); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("GetDocumentByID after delete = %v, want ErrDocumentNotFound", err)
	}
	for _, table := range []string{"chunks", "parent_chunks", "document_principals", "document_groups"} {
		var rows int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE document_id = $1", doc.ID).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if rows != 0 {
			t.Errorf("%s holds %d rows for the deleted Document, want 0 by cascade", table, rows)
		}
	}
	if _, err := docs.DeleteDocument(ctx, uuid.New()); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("DeleteDocument(unknown) = %v, want ErrDocumentNotFound", err)
	}
}
