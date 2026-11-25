package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/auth"
)

// keyring is an Authenticator over a fixed key → Principal map.
type keyring map[string]auth.Principal

func (k keyring) Authenticate(_ context.Context, key string) (auth.Principal, error) {
	p, ok := k[key]
	if !ok {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return p, nil
}

// failing is an Authenticator whose backing store is down.
type failing struct{}

func (failing) Authenticate(context.Context, string) (auth.Principal, error) {
	return auth.Principal{}, errors.New("connection refused")
}

var alice = auth.Principal{
	ID:   uuid.MustParse("0190d2a8-0000-7000-8000-000000000001"),
	Name: "alice",
	Groups: []auth.Group{
		{ID: uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a1"), Name: "legal"},
		{ID: uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a2"), Name: "tax"},
	},
}

func get(t *testing.T, h http.Handler, path, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMeReturnsTheCallingPrincipalAndGroups(t *testing.T) {
	h := New(Readiness{}, keyring{"trelis_alice": alice})
	rec := get(t, h, "/me", "Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me = %d %s, want 200", rec.Code, rec.Body)
	}
	want := `{"id":"0190d2a8-0000-7000-8000-000000000001","name":"alice","groups":[` +
		`{"id":"0190d2a8-0000-7000-8000-0000000000a1","name":"legal"},` +
		`{"id":"0190d2a8-0000-7000-8000-0000000000a2","name":"tax"}]}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %s\nwant   %s", rec.Body, want)
	}
}

func TestRequestsWithoutAValidKeyAre401(t *testing.T) {
	h := New(Readiness{}, keyring{"trelis_alice": alice})
	for _, authz := range []string{"", "Bearer", "Bearer ", "Bearer trelis_wrong", "Basic trelis_alice", "trelis_alice"} {
		rec := get(t, h, "/me", authz)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status = %d, want 401", authz, rec.Code)
			continue
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("Authorization %q: WWW-Authenticate = %q, want Bearer", authz, got)
		}
		var body struct{ Error string }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
			t.Errorf("Authorization %q: body = %s, want a JSON error", authz, rec.Body)
		}
	}
}

func TestAnAuthenticatorFailureIs500Not401(t *testing.T) {
	rec := get(t, New(Readiness{}, failing{}), "/me", "Bearer trelis_alice")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the key cannot be checked", rec.Code)
	}
}

func TestProbesNeedNoKey(t *testing.T) {
	h := New(Readiness{}, keyring{})
	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := get(t, h, path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s without a key = %d, want 200", path, rec.Code)
		}
	}
}
