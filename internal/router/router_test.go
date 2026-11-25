package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthzReturnsOK(t *testing.T) {
	rec := httptest.NewRecorder()
	New(Readiness{}, keyring{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body := rec.Body.String(); body != `{"status":"ok"}`+"\n" {
		t.Errorf("body = %q, want {\"status\":\"ok\"}", body)
	}
}

func TestUnknownRouteIs404WithAKey(t *testing.T) {
	rec := get(t, New(Readiness{}, keyring{"trelis_alice": alice}), "/nope", "Bearer trelis_alice")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestUnknownRoutesAndMethodsNeedAKey(t *testing.T) {
	h := New(Readiness{}, keyring{"trelis_alice": alice})
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/nope", nil),
		httptest.NewRequest(http.MethodDelete, "/me", nil),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a key = %d, want 401 (routes must not be discoverable)", req.Method, req.URL.Path, rec.Code)
		}
	}
}
