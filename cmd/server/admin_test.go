package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/phongnd2802/trelis/internal/auth"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
)

func runAdmin(t *testing.T, s *auth.Store, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := admin(context.Background(), s, args, &out)
	return out.String(), err
}

func TestAdminCreatesEverythingAKeyNeeds(t *testing.T) {
	s := auth.NewStore(dbtest.Migrated(t))
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

	key := strings.TrimSpace(out)
	p, err := s.Authenticate(context.Background(), key)
	if err != nil {
		t.Fatalf("the printed key %q does not authenticate: %v", key, err)
	}
	if p.Name != "alice" || len(p.Groups) != 1 || p.Groups[0].Name != "tax" {
		t.Errorf("key authenticates as %+v, want alice in tax", p)
	}
}

func TestAdminRejectsBadUsage(t *testing.T) {
	s := auth.NewStore(dbtest.Migrated(t))
	for _, args := range [][]string{
		nil,
		{"principal"},
		{"principal", "create"},
		{"principal", "create", "a", "b"},
		{"group", "add-member", "tax"},
		{"key", "delete", "alice"},
		{"nope"},
	} {
		if _, err := runAdmin(t, s, args...); !errors.Is(err, errUsage) {
			t.Errorf("admin %q: err = %v, want a usage error", args, err)
		}
	}
}

func TestAdminReportsUnknownNames(t *testing.T) {
	s := auth.NewStore(dbtest.Migrated(t))
	if _, err := runAdmin(t, s, "key", "create", "ghost"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("key for unknown principal: err = %v, want ErrNotFound", err)
	}
}
