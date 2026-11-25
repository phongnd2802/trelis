package auth_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/phongnd2802/trelis/internal/auth"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
)

func newStore(t *testing.T) *auth.Store {
	t.Helper()
	return auth.NewStore(dbtest.Migrated(t))
}

func TestAKeyAuthenticatesAsItsPrincipal(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	alice, err := s.CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	key, err := s.CreateKey(ctx, "alice")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	got, err := s.Authenticate(ctx, key)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != alice.ID || got.Name != "alice" {
		t.Fatalf("Authenticate = %+v, want alice (%s)", got, alice.ID)
	}
}

func TestAnUnknownKeyIsUnauthenticated(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.CreatePrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	key, err := s.CreateKey(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"", "trelis_nope", key + "x"} {
		if _, err := s.Authenticate(ctx, bad); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) err = %v, want ErrUnauthenticated", bad, err)
		}
	}
}

func TestAuthenticateReturnsThePrincipalsGroups(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, name := range []string{"alice", "bob"} {
		if _, err := s.CreatePrincipal(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"tax", "legal", "hr"} {
		if _, err := s.CreateGroup(ctx, name); err != nil {
			t.Fatalf("CreateGroup(%s): %v", name, err)
		}
	}
	for _, m := range [][2]string{{"tax", "alice"}, {"legal", "alice"}, {"hr", "bob"}, {"tax", "alice"}} {
		if err := s.AddMember(ctx, m[0], m[1]); err != nil {
			t.Fatalf("AddMember(%s, %s): %v", m[0], m[1], err)
		}
	}

	aliceKey, err := s.CreateKey(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	alice, err := s.Authenticate(ctx, aliceKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := groupNames(alice); !slices.Equal(got, []string{"legal", "tax"}) {
		t.Errorf("alice's groups = %v, want [legal tax]", got)
	}
}

func TestAPrincipalWithNoGroupsHasAnEmptyList(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.CreatePrincipal(ctx, "carol"); err != nil {
		t.Fatal(err)
	}
	key, err := s.CreateKey(ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	carol, err := s.Authenticate(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if carol.Groups == nil || len(carol.Groups) != 0 {
		t.Errorf("carol.Groups = %#v, want an empty, non-nil list", carol.Groups)
	}
}

func TestAddMemberRejectsUnknownNames(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.CreatePrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGroup(ctx, "tax"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "nope", "alice"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("unknown group: err = %v, want ErrNotFound", err)
	}
	if err := s.AddMember(ctx, "tax", "nope"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("unknown principal: err = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateKey(ctx, "nope"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("key for unknown principal: err = %v, want ErrNotFound", err)
	}
}

func groupNames(p auth.Principal) []string {
	names := make([]string, 0, len(p.Groups))
	for _, g := range p.Groups {
		names = append(names, g.Name)
	}
	return names
}

func TestKeysAreStoredHashed(t *testing.T) {
	pool := dbtest.Migrated(t)
	s := auth.NewStore(pool)
	ctx := context.Background()
	if _, err := s.CreatePrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	key, err := s.CreateKey(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}

	// The acceptance criterion is about what sits in the table, so read it directly.
	var rows int
	var leaked bool
	err = pool.QueryRow(ctx, `
		SELECT count(*),
		       coalesce(bool_or(row_to_json(k)::text LIKE '%' || $1 || '%'
		                        OR position(convert_to($1, 'UTF8') IN k.key_hash) > 0), false)
		FROM api_keys k`, key).Scan(&rows, &leaked)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 || leaked {
		t.Fatalf("api_keys has %d rows, plaintext key present = %v; want 1 row and no plaintext", rows, leaked)
	}
}

func TestNamesAreUnique(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.CreatePrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePrincipal(ctx, "alice"); !errors.Is(err, auth.ErrExists) {
		t.Errorf("second principal alice: err = %v, want ErrExists", err)
	}
	if _, err := s.CreateGroup(ctx, "tax"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGroup(ctx, "tax"); !errors.Is(err, auth.ErrExists) {
		t.Errorf("second group tax: err = %v, want ErrExists", err)
	}
}
