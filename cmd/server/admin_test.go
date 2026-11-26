package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/application/service"
	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// services is what the admin commands run through, plus the AuthService to
// check that the keys they print work.
type services struct {
	interfaces.PrincipalService
	interfaces.AuthService
}

func newServices(t *testing.T) services {
	t.Helper()
	store := db.NewStore(dbtest.Migrated(t))
	principals, keys := repository.NewPrincipalRepository(store), repository.NewAPIKeyRepository(store)
	return services{service.NewPrincipalService(store, principals, keys), service.NewAuthService(principals, keys)}
}

func runAdmin(t *testing.T, s services, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := admin(context.Background(), s, args, &out)
	return out.String(), err
}

func TestAdminCreatesEverythingAKeyNeeds(t *testing.T) {
	s := newServices(t)
	for _, args := range [][]string{
		{"principal", "create", "alice"},
		{"group", "create", "tax"},
		{"group", "add-member", "tax", "alice"},
	} {
		if _, err := runAdmin(t, s, args...); err != nil {
			t.Fatalf("admin %v: %v", args, err)
		}
	}
	out, err := runAdmin(t, s, "key", "create", "alice")
	if err != nil {
		t.Fatalf("admin key create: %v", err)
	}

	_, key := createdKey(t, out)
	p, err := s.Authenticate(context.Background(), key)
	if err != nil {
		t.Fatalf("the printed key %q does not authenticate: %v", key, err)
	}
	if p.Name != "alice" || len(p.Groups) != 1 || p.Groups[0].Name != "tax" {
		t.Errorf("key authenticates as %+v, want alice in tax", p)
	}
}

func TestAdminRejectsBadUsage(t *testing.T) {
	s := newServices(t)
	for _, args := range [][]string{
		nil,
		{"principal"},
		{"principal", "create"},
		{"principal", "create", "a", "b"},
		{"group", "add-member", "tax"},
		{"key", "delete", "alice"},
		{"key", "list"},
		{"key", "revoke"},
		{"key", "revoke", "a", "b"},
		{"nope"},
	} {
		if _, err := runAdmin(t, s, args...); !errors.Is(err, errUsage) {
			t.Errorf("admin %q: err = %v, want a usage error", args, err)
		}
	}
}

func TestAdminReportsUnknownNames(t *testing.T) {
	s := newServices(t)
	if _, err := runAdmin(t, s, "key", "create", "ghost"); !errors.Is(err, repository.ErrPrincipalNotFound) {
		t.Errorf("key for unknown principal: err = %v, want ErrPrincipalNotFound", err)
	}
}

// createdKey splits `key create` output, "<id>\t<key>\n", into its columns.
func createdKey(t *testing.T, out string) (id, key string) {
	t.Helper()
	id, key, ok := strings.Cut(strings.TrimSuffix(out, "\n"), "\t")
	if !ok || id == "" || !strings.HasPrefix(key, "trelis_") || strings.Contains(key, "\n") {
		t.Fatalf("key create printed %q, want one \"<id>\\t<key>\" line", out)
	}
	return id, key
}

func TestAdminListsAndRevokesKeys(t *testing.T) {
	s := newServices(t)
	ctx := context.Background()
	if _, err := runAdmin(t, s, "principal", "create", "alice"); err != nil {
		t.Fatal(err)
	}
	out, err := runAdmin(t, s, "key", "create", "alice")
	if err != nil {
		t.Fatal(err)
	}
	leakedID, leakedKey := createdKey(t, out)
	out, err = runAdmin(t, s, "key", "create", "alice")
	if err != nil {
		t.Fatal(err)
	}
	otherID, otherKey := createdKey(t, out)

	if _, err := runAdmin(t, s, "key", "revoke", leakedID); err != nil {
		t.Fatalf("key revoke: %v", err)
	}
	if _, err := s.Authenticate(ctx, leakedKey); !errors.Is(err, interfaces.ErrUnauthenticated) {
		t.Errorf("revoked key: err = %v, want ErrUnauthenticated", err)
	}
	if _, err := s.Authenticate(ctx, otherKey); err != nil {
		t.Errorf("other key stopped working: %v", err)
	}

	list, err := runAdmin(t, s, "key", "list", "alice")
	if err != nil {
		t.Fatalf("key list: %v", err)
	}
	if strings.Contains(list, leakedKey) || strings.Contains(list, otherKey) || strings.Contains(list, "trelis_") {
		t.Fatalf("key list printed a key:\n%s", list)
	}
	lines := strings.Split(strings.TrimSuffix(list, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("key list printed %d lines, want 2:\n%s", len(lines), list)
	}
	if !strings.HasPrefix(lines[0], leakedID+"\t") || !strings.Contains(lines[0], "\trevoked ") {
		t.Errorf("line for the revoked key = %q, want %s ... revoked <time>", lines[0], leakedID)
	}
	if !strings.HasPrefix(lines[1], otherID+"\t") || !strings.HasSuffix(lines[1], "\tactive") {
		t.Errorf("line for the active key = %q, want %s ... active", lines[1], otherID)
	}
}

func TestAdminRevokeRejectsBadIDs(t *testing.T) {
	s := newServices(t)
	if _, err := runAdmin(t, s, "key", "revoke", "not-a-uuid"); err == nil || errors.Is(err, repository.ErrAPIKeyNotFound) {
		t.Errorf("malformed ID: err = %v, want a parse error", err)
	}
	if _, err := runAdmin(t, s, "key", "revoke", "0190d2a8-0000-7000-8000-000000000001"); !errors.Is(err, repository.ErrAPIKeyNotFound) {
		t.Errorf("unknown ID: err = %v, want ErrAPIKeyNotFound", err)
	}
}
