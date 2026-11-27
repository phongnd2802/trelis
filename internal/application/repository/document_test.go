package repository

import (
	"context"
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
