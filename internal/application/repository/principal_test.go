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

func TestPrincipalAndGroupNamesAreUnique(t *testing.T) {
	s, _ := seeded(t)
	repo, ctx := NewPrincipalRepository(s), context.Background()
	if _, err := repo.CreatePrincipal(ctx, "alice"); !errors.Is(err, ErrPrincipalExists) {
		t.Errorf("second principal alice: err = %v, want ErrPrincipalExists", err)
	}
	if _, err := repo.CreateGroup(ctx, "tax"); !errors.Is(err, ErrGroupExists) {
		t.Errorf("second group tax: err = %v, want ErrGroupExists", err)
	}
}

func TestLookupsByNameReportUnknownNames(t *testing.T) {
	s, ids := seeded(t)
	repo, ctx := NewPrincipalRepository(s), context.Background()
	if p, err := repo.GetPrincipalByName(ctx, "bob"); err != nil || p.ID != ids["bob"] {
		t.Errorf("GetPrincipalByName(bob) = %+v, %v; want bob", p, err)
	}
	if g, err := repo.GetGroupByName(ctx, "legal"); err != nil || g.ID != ids["legal"] {
		t.Errorf("GetGroupByName(legal) = %+v, %v; want legal", g, err)
	}
	if _, err := repo.GetPrincipalByName(ctx, "ghost"); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("GetPrincipalByName(ghost): err = %v, want ErrPrincipalNotFound", err)
	}
	if _, err := repo.GetGroupByName(ctx, "nobody"); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("GetGroupByName(nobody): err = %v, want ErrGroupNotFound", err)
	}
}

func TestListGroupsReturnsMembershipsByName(t *testing.T) {
	s, ids := seeded(t)
	repo, ctx := NewPrincipalRepository(s), context.Background()
	for _, m := range [][2]string{{"tax", "alice"}, {"legal", "alice"}, {"tax", "alice"}} {
		if err := repo.AddMember(ctx, ids[m[0]], ids[m[1]]); err != nil {
			t.Fatalf("AddMember(%s, %s): %v", m[0], m[1], err)
		}
	}
	got, err := repo.ListGroups(ctx, ids["alice"])
	if err != nil {
		t.Fatal(err)
	}
	want := []types.Group{{ID: ids["legal"], Name: "legal"}, {ID: ids["tax"], Name: "tax"}}
	if !slices.Equal(got, want) {
		t.Errorf("alice's groups = %+v, want %+v", got, want)
	}
	bobs, err := repo.ListGroups(ctx, ids["bob"])
	if err != nil || bobs == nil || len(bobs) != 0 {
		t.Errorf("bob's groups = %#v, %v; want an empty, non-nil list", bobs, err)
	}
}
