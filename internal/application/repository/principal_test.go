package repository

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/types"
)

// seeded is a Store with Principals alice and bob and Groups tax and legal.
func seeded(t *testing.T) (db.Store, map[string]uuid.UUID) {
	t.Helper()
	s, ctx := db.NewStore(dbtest.Migrated(t)), context.Background()
	q := s.Querier(ctx)
	ids := map[string]uuid.UUID{}
	for _, name := range []string{"alice", "bob"} {
		p, err := q.CreatePrincipal(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = p.ID
	}
	for _, name := range []string{"tax", "legal"} {
		g, err := q.CreateGroup(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = g.ID
	}
	return s, ids
}

func TestResolveNamesReturnsTheIDsOfEveryName(t *testing.T) {
	s, ids := seeded(t)
	got, err := NewPrincipalRepository(s).ResolveNames(context.Background(),
		types.AccessList{Principals: []string{"bob", "alice"}, Groups: []string{"legal"}})
	if err != nil {
		t.Fatalf("ResolveNames: %v", err)
	}
	if !slices.Equal(got.PrincipalIDs, []uuid.UUID{ids["bob"], ids["alice"]}) || !slices.Equal(got.GroupIDs, []uuid.UUID{ids["legal"]}) {
		t.Errorf("ResolveNames = %+v, want bob, alice and legal in order", got)
	}
}

func TestResolveNamesOfAnEmptyListIsEmpty(t *testing.T) {
	s, _ := seeded(t)
	got, err := NewPrincipalRepository(s).ResolveNames(context.Background(), types.AccessList{})
	if err != nil || len(got.PrincipalIDs)+len(got.GroupIDs) != 0 {
		t.Errorf("ResolveNames(empty) = %+v, %v; want nothing", got, err)
	}
}

func TestResolveNamesNamesTheUnknownEntry(t *testing.T) {
	s, _ := seeded(t)
	repo := NewPrincipalRepository(s)
	for _, tc := range []struct {
		list types.AccessList
		want error
		name string
	}{
		{types.AccessList{Principals: []string{"alice", "ghost"}}, ErrPrincipalNotFound, "ghost"},
		{types.AccessList{Groups: []string{"tax", "nobody"}}, ErrGroupNotFound, "nobody"},
	} {
		_, err := repo.ResolveNames(context.Background(), tc.list)
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("ResolveNames(%+v) = %v, want %v naming %q", tc.list, err, tc.want, tc.name)
		}
	}
}
