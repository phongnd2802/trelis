package repository

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

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

func TestListPendingDocumentIDsReturnsOnlyPendingDocuments(t *testing.T) {
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

	got, err := docs.ListPendingDocumentIDs(ctx)
	if err != nil {
		t.Fatalf("ListPendingDocumentIDs: %v", err)
	}
	if want := []uuid.UUID{pending.ID}; !slices.Equal(got, want) {
		t.Errorf("pending IDs = %v, want %v", got, want)
	}
}

func TestListDocumentUploadIDsReturnsTheCurrentUploadOfEachDocument(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	docs := NewDocumentRepository(s)
	aliceDoc, bobDoc := document(t, s, ids["alice"]), document(t, s, ids["bob"])

	got, err := docs.ListDocumentUploadIDs(ctx, []uuid.UUID{aliceDoc.ID, bobDoc.ID, uuid.New()})
	if err != nil {
		t.Fatalf("ListDocumentUploadIDs: %v", err)
	}
	want := map[uuid.UUID]uuid.UUID{aliceDoc.ID: aliceDoc.UploadID, bobDoc.ID: bobDoc.UploadID}
	if !maps.Equal(got, want) {
		t.Errorf("upload IDs = %v, want %v", got, want)
	}

	got, err = docs.ListDocumentUploadIDs(ctx, nil)
	if err != nil || len(got) != 0 {
		t.Errorf("ListDocumentUploadIDs(nil) = %v, %v; want an empty map and no error", got, err)
	}
}
