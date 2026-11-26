package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestAKeyHashFindsItsPrincipalUntilRevoked(t *testing.T) {
	s, ids := seeded(t)
	keys, ctx := NewAPIKeyRepository(s), context.Background()
	leaked, err := keys.CreateAPIKey(ctx, ids["alice"], []byte("hash-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.CreateAPIKey(ctx, ids["alice"], []byte("hash-2")); err != nil {
		t.Fatal(err)
	}

	p, err := keys.GetPrincipalByKeyHash(ctx, []byte("hash-1"))
	if err != nil || p.ID != ids["alice"] || p.Name != "alice" {
		t.Fatalf("GetPrincipalByKeyHash = %+v, %v; want alice", p, err)
	}
	if _, err := keys.GetPrincipalByKeyHash(ctx, []byte("hash-3")); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("unknown hash: err = %v, want ErrAPIKeyNotFound", err)
	}

	if err := keys.RevokeAPIKey(ctx, leaked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.GetPrincipalByKeyHash(ctx, []byte("hash-1")); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("revoked key: err = %v, want ErrAPIKeyNotFound", err)
	}
	if _, err := keys.GetPrincipalByKeyHash(ctx, []byte("hash-2")); err != nil {
		t.Errorf("other key: %v", err)
	}
}

func TestListAPIKeysShowsRevocationOldestFirst(t *testing.T) {
	s, ids := seeded(t)
	keys, ctx := NewAPIKeyRepository(s), context.Background()
	first, err := keys.CreateAPIKey(ctx, ids["alice"], []byte("hash-1"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := keys.CreateAPIKey(ctx, ids["alice"], []byte("hash-2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.RevokeAPIKey(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	list, err := keys.ListAPIKeys(ctx, ids["alice"])
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != first.ID || list[0].RevokedAt == nil ||
		list[1].ID != second.ID || list[1].RevokedAt != nil {
		t.Errorf("ListAPIKeys = %+v, want first (revoked) then second (active)", list)
	}
	if bobs, err := keys.ListAPIKeys(ctx, ids["bob"]); err != nil || len(bobs) != 0 {
		t.Errorf("bob's keys = %+v, %v; want none", bobs, err)
	}
}

func TestRevokingAnUnknownKeyIsNotFound(t *testing.T) {
	s, _ := seeded(t)
	err := NewAPIKeyRepository(s).RevokeAPIKey(context.Background(), uuid.MustParse("0190d2a8-0000-7000-8000-000000000001"))
	if !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("err = %v, want ErrAPIKeyNotFound", err)
	}
}
