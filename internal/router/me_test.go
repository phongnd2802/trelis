package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/service"
	"github.com/phongnd2802/trelis/internal/handler"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// keyring is an AuthService over a fixed key → Principal map.
type keyring map[string]types.Principal

func (k keyring) Authenticate(_ context.Context, key string) (types.Principal, error) {
	p, ok := k[key]
	if !ok {
		return types.Principal{}, interfaces.ErrUnauthenticated
	}
	return p, nil
}

// failing is an AuthService whose backing store is down.
type failing struct{}

func (failing) Authenticate(context.Context, string) (types.Principal, error) {
	return types.Principal{}, errors.New("connection refused")
}

var alice = types.Principal{
	ID:   uuid.MustParse("0190d2a8-0000-7000-8000-000000000001"),
	Name: "alice",
	Groups: []types.Group{
		{ID: uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a1"), Name: "legal"},
		{ID: uuid.MustParse("0190d2a8-0000-7000-8000-0000000000a2"), Name: "tax"},
	},
}

// newRouter is the router on authn, without Swagger. GET /me reads only the
// request context, so its PrincipalService needs no repositories.
func newRouter(authn interfaces.AuthService) http.Handler {
	return newRouterWith(authn, false)
}

// newRouterWith is the router on authn, with Swagger mounted when swagger is true.
func newRouterWith(authn interfaces.AuthService, swagger bool) http.Handler {
	me := handler.NewPrincipalHandler(service.NewPrincipalService(nil, nil, nil))
	return New(Readiness{}, authn, me, nil, nil, swagger)
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
	h := newRouter(keyring{"trelis_alice": alice})
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
	h := newRouter(keyring{"trelis_alice": alice})
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
	rec := get(t, newRouter(failing{}), "/me", "Bearer trelis_alice")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the key cannot be checked", rec.Code)
	}
}

func TestProbesNeedNoKey(t *testing.T) {
	h := newRouter(keyring{})
	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := get(t, h, path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s without a key = %d, want 200", path, rec.Code)
		}
	}
}
