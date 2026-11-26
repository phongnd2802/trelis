package router

import (
	"context"
	"net/http"
	"testing"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/handler"
)

// Against the real store: revocation takes effect on the very next request.
func TestARevokedKeyGets401OnTheNextRequest(t *testing.T) {
	authn, store := realAuth(db.NewStore(dbtest.Migrated(t)))
	ctx := context.Background()
	if _, err := store.CreatePrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	leaked, leakedKey, err := store.CreateAPIKey(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, err := store.CreateAPIKey(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	h := New(Readiness{}, authn, handler.NewPrincipalHandler(store), nil, false)

	if rec := get(t, h, "/me", "Bearer "+leakedKey); rec.Code != http.StatusOK {
		t.Fatalf("before revoke: GET /me = %d, want 200", rec.Code)
	}
	if err := store.RevokeAPIKey(ctx, leaked.ID); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, h, "/me", "Bearer "+leakedKey); rec.Code != http.StatusUnauthorized {
		t.Errorf("after revoke: GET /me = %d, want 401", rec.Code)
	}
	if rec := get(t, h, "/me", "Bearer "+otherKey); rec.Code != http.StatusOK {
		t.Errorf("alice's other key: GET /me = %d, want 200", rec.Code)
	}
}
