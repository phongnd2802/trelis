package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func up(context.Context) error { return nil }

func down(msg string) func(context.Context) error {
	return func(context.Context) error { return errors.New(msg) }
}

func hang(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

type checkResult struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

type readyBody struct {
	Status string                 `json:"status"`
	Checks map[string]checkResult `json:"checks"`
}

func getReady(t *testing.T, r Readiness) (int, readyBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	New(r, keyring{}, nil, nil, nil, nil, nil, nil, false).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body readyBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestReadyzAllUp(t *testing.T) {
	code, body := getReady(t, Readiness{Timeout: time.Second, Checks: []Check{
		{Name: "postgres", Probe: up},
		{Name: "neo4j", Probe: up},
		{Name: "temporal", Probe: up},
	}})
	if code != http.StatusOK || body.Status != "ready" {
		t.Fatalf("got %d %q, want 200 ready", code, body.Status)
	}
	for _, name := range []string{"postgres", "neo4j", "temporal"} {
		if body.Checks[name].Status != "up" {
			t.Errorf("checks[%s] = %+v, want up", name, body.Checks[name])
		}
	}
}

func TestReadyzReportsEachDependencyDown(t *testing.T) {
	code, body := getReady(t, Readiness{Timeout: time.Second, Checks: []Check{
		{Name: "postgres", Probe: up},
		{Name: "neo4j", Probe: down("connection refused")},
		{Name: "temporal", Probe: up},
	}})
	if code != http.StatusServiceUnavailable || body.Status != "unready" {
		t.Fatalf("got %d %q, want 503 unready", code, body.Status)
	}
	if got := body.Checks["neo4j"]; got.Status != "down" || got.Error != "connection refused" {
		t.Errorf("checks[neo4j] = %+v, want down with the probe error", got)
	}
	if body.Checks["postgres"].Status != "up" || body.Checks["temporal"].Status != "up" {
		t.Errorf("healthy checks not reported up: %+v", body.Checks)
	}
}

func TestReadyzTimesOutAHangingProbe(t *testing.T) {
	start := time.Now()
	code, body := getReady(t, Readiness{Timeout: 50 * time.Millisecond, Checks: []Check{
		{Name: "temporal", Probe: hang},
		{Name: "postgres", Probe: up},
	}})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("readyz took %v, want it bounded by the 50ms timeout", elapsed)
	}
	if code != http.StatusServiceUnavailable || body.Checks["temporal"].Status != "down" {
		t.Fatalf("got %d %+v, want 503 with temporal down", code, body.Checks)
	}
	if body.Checks["postgres"].Status != "up" {
		t.Errorf("checks[postgres] = %+v, want up", body.Checks["postgres"])
	}
}

func TestHealthzStaysUpWhenDependenciesAreDown(t *testing.T) {
	rec := httptest.NewRecorder()
	r := Readiness{Timeout: time.Second, Checks: []Check{{Name: "postgres", Probe: down("nope")}}}
	New(r, keyring{}, nil, nil, nil, nil, nil, nil, false).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 (liveness ignores dependencies)", rec.Code)
	}
}
