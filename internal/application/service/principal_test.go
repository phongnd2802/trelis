package service_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/application/service"
	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// services is the Principal and Auth services on one database, so a test can
// issue a key and then authenticate with it.
type services struct {
	interfaces.PrincipalService
	interfaces.AuthService
}

func newServicesOn(pool *pgxpool.Pool) services {
	store := db.NewStore(pool)
	principals, keys := repository.NewPrincipalRepository(store), repository.NewAPIKeyRepository(store)
	return services{
		PrincipalService: service.NewPrincipalService(store, principals, keys),
		AuthService:      service.NewAuthService(principals, keys),
	}
}

func newServices(t *testing.T) services {
	t.Helper()
	return newServicesOn(dbtest.Migrated(t))
}

func TestAKeyAuthenticatesAsItsPrincipal(t *testing.T) {
	s := newServices(t)
	ctx := context.Background()
	alice, err := s.CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	_, key, err := s.CreateAPIKey(ctx, "alice")
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
	s := newServices(t)
	ctx := context.Background()
	if _, err := s.CreatePrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	_, key, err := s.CreateAPIKey(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"", "trelis_nope", key + "x"} {
		if _, err := s.Authenticate(ctx, bad); !errors.Is(err, interfaces.ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) err = %v, want ErrUnauthenticated", bad, err)
		}
	}
}

func TestAuthenticateReturnsThePrincipalsGroups(t *testing.T) {
	s := newServices(t)
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

	_, aliceKey, err := s.CreateAPIKey(ctx, "alice")
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
	s := newServices(t)
	ctx := context.Background()
	if _, err := s.CreatePrincipal(ctx, "carol"); err != nil {
		t.Fatal(err)
	}
	_, key, err := s.CreateAPIKey(ctx, "carol")
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
	s := newServices(t)
	ctx := context.Background()
	if _, err := s.CreatePrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGroup(ctx, "tax"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "nope", "alice"); !errors.Is(err, repository.ErrGroupNotFound) {
		t.Errorf("unknown group: err = %v, want ErrGroupNotFound", err)
	}
	if err := s.AddMember(ctx, "tax", "nope"); !errors.Is(err, repository.ErrPrincipalNotFound) {
		t.Errorf("unknown principal: err = %v, want ErrPrincipalNotFound", err)
	}
	if _, _, err := s.CreateAPIKey(ctx, "nope"); !errors.Is(err, repository.ErrPrincipalNotFound) {
		t.Errorf("key for unknown principal: err = %v, want ErrPrincipalNotFound", err)
	}
}

func groupNames(p types.Principal) []string {
	names := make([]string, 0, len(p.Groups))
	for _, g := range p.Groups {
		names = append(names, g.Name)
	}
	return names
}

func TestKeysAreStoredHashed(t *testing.T) {
	pool := dbtest.Migrated(t)
	s := newServicesOn(pool)
	ctx := context.Background()
	if _, err := s.CreatePrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	_, key, err := s.CreateAPIKey(ctx, "alice")
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
	s := newServices(t)
	ctx := context.Background()
	if _, err := s.CreatePrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePrincipal(ctx, "alice"); !errors.Is(err, repository.ErrPrincipalExists) {
		t.Errorf("second principal alice: err = %v, want ErrPrincipalExists", err)
	}
	if _, err := s.CreateGroup(ctx, "tax"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGroup(ctx, "tax"); !errors.Is(err, repository.ErrGroupExists) {
		t.Errorf("second group tax: err = %v, want ErrGroupExists", err)
	}
}

func TestARevokedKeyNoLongerAuthenticates(t *testing.T) {
	s := newServices(t)
	ctx := context.Background()
	if _, err := s.CreatePrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	leaked, leakedKey, err := s.CreateAPIKey(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, err := s.CreateAPIKey(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.RevokeAPIKey(ctx, leaked.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := s.Authenticate(ctx, leakedKey); !errors.Is(err, interfaces.ErrUnauthenticated) {
		t.Errorf("revoked key: err = %v, want ErrUnauthenticated", err)
	}
	if p, err := s.Authenticate(ctx, otherKey); err != nil || p.Name != "alice" {
		t.Errorf("alice's other key = (%+v, %v), want it to still authenticate", p, err)
	}
}

func TestListKeysShowsEachKeyAndWhetherItIsRevoked(t *testing.T) {
	s := newServices(t)
	ctx := context.Background()
	for _, name := range []string{"alice", "bob"} {
		if _, err := s.CreatePrincipal(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	first, _, err := s.CreateAPIKey(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.CreateAPIKey(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAPIKey(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAPIKey(ctx, first.ID); err != nil {
		t.Fatal(err)
	}

	keys, err := s.ListAPIKeys(ctx, "alice")
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 2 || keys[0].ID != first.ID || keys[1].ID != second.ID {
		t.Fatalf("ListKeys(alice) = %+v, want alice's two keys, oldest first", keys)
	}
	if keys[0].RevokedAt == nil || keys[1].RevokedAt != nil {
		t.Errorf("revoked = [%v %v], want [revoked, active]", keys[0].RevokedAt, keys[1].RevokedAt)
	}
	if keys[1].CreatedAt.IsZero() || !keys[1].CreatedAt.Equal(second.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v from CreateKey", keys[1].CreatedAt, second.CreatedAt)
	}

	if _, err := s.ListAPIKeys(ctx, "ghost"); !errors.Is(err, repository.ErrPrincipalNotFound) {
		t.Errorf("ListAPIKeys(ghost) err = %v, want ErrPrincipalNotFound", err)
	}
}

func TestRevokingTwiceKeepsTheFirstRevocation(t *testing.T) {
	s := newServices(t)
	ctx := context.Background()
	if _, err := s.CreatePrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	k, _, err := s.CreateAPIKey(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAPIKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	before, err := s.ListAPIKeys(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAPIKey(ctx, k.ID); err != nil {
		t.Fatalf("second RevokeKey: %v, want a no-op", err)
	}
	after, err := s.ListAPIKeys(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !after[0].RevokedAt.Equal(*before[0].RevokedAt) {
		t.Errorf("RevokedAt moved from %v to %v", before[0].RevokedAt, after[0].RevokedAt)
	}
}

func TestRevokingAnUnknownKeyIsNotFound(t *testing.T) {
	s := newServices(t)
	if err := s.RevokeAPIKey(context.Background(), uuid.New()); !errors.Is(err, repository.ErrAPIKeyNotFound) {
		t.Errorf("err = %v, want ErrAPIKeyNotFound", err)
	}
}

func TestGetCurrentPrincipalIsTheAuthenticatedOne(t *testing.T) {
	s := newServices(t)
	alice := types.Principal{ID: uuid.New(), Name: "alice", Groups: []types.Group{}}
	got, err := s.GetCurrentPrincipal(types.WithPrincipal(context.Background(), alice))
	if err != nil || got.ID != alice.ID || got.Name != "alice" {
		t.Errorf("GetCurrentPrincipal = %+v, %v; want alice", got, err)
	}
	if _, err := s.GetCurrentPrincipal(context.Background()); err == nil {
		t.Error("GetCurrentPrincipal without a Principal in ctx succeeded")
	}
}
