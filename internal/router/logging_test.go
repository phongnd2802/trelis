package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// observeLogs replaces the global logger for the test and returns what it saw.
func observeLogs(t *testing.T) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core, zap.AddStacktrace(zapcore.ErrorLevel)))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })
	return logs
}

// requestLine is the one "http request" entry whose route is route.
func requestLine(t *testing.T, logs *observer.ObservedLogs, route string) observer.LoggedEntry {
	t.Helper()
	for _, entry := range logs.FilterMessage("http request").All() {
		if entry.ContextMap()["route"] == route {
			return entry
		}
	}
	t.Fatalf("no http request line for route %q; log was:\n%v", route, logs.All())
	return observer.LoggedEntry{}
}

func TestEveryResponseGetsAServerGeneratedRequestID(t *testing.T) {
	observeLogs(t)
	h := newRouter(keyring{"trelis_alice": alice})

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer trelis_alice")
	req.Header.Set("X-Request-Id", "evil")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	id := rec.Header().Get("X-Request-Id")
	if id == "evil" {
		t.Fatal("the incoming X-Request-Id was echoed")
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("X-Request-Id = %q, want a UUID: %v", id, err)
	}

	second := get(t, h, "/me", "Bearer trelis_alice").Header().Get("X-Request-Id")
	if second == id {
		t.Errorf("two requests got the same X-Request-Id %q", id)
	}
}

func TestAccessLogHasTheRoutePatternAndTheCallersPrincipal(t *testing.T) {
	logs := observeLogs(t)
	h := newRouter(keyring{"trelis_alice": alice})

	rec := get(t, h, "/documents/not-a-uuid", "Bearer trelis_alice")
	entry := requestLine(t, logs, "/documents/{id}")
	fields := entry.ContextMap()
	if fields["status"] != int64(http.StatusNotFound) {
		t.Errorf("status = %v, want 404", fields["status"])
	}
	if entry.Level != zapcore.WarnLevel {
		t.Errorf("level = %v, want WARN for a 4xx", entry.Level)
	}
	if fields["request_id"] != rec.Header().Get("X-Request-Id") {
		t.Errorf("request_id = %v, want the X-Request-Id header %q", fields["request_id"], rec.Header().Get("X-Request-Id"))
	}
	if fields["principal_id"] != alice.ID.String() {
		t.Errorf("principal_id = %v, want %s", fields["principal_id"], alice.ID)
	}
	if _, ok := fields["duration_ms"]; !ok {
		t.Error("duration_ms is missing")
	}
	if _, ok := fields["bytes"]; !ok {
		t.Error("bytes is missing")
	}
}

func TestUnauthorizedRequestLogsAtWarn(t *testing.T) {
	logs := observeLogs(t)
	get(t, newRouter(keyring{}), "/me", "")

	entry := requestLine(t, logs, "/me")
	if entry.Level != zapcore.WarnLevel {
		t.Errorf("level = %v, want WARN", entry.Level)
	}
	if got := entry.ContextMap()["status"]; got != int64(http.StatusUnauthorized) {
		t.Errorf("status = %v, want 401", got)
	}
}

func TestUnknownPathLogsAPlaceholderRoute(t *testing.T) {
	logs := observeLogs(t)
	get(t, newRouter(keyring{"trelis_alice": alice}), "/nope", "Bearer trelis_alice")

	entry := requestLine(t, logs, unmatchedRoute)
	fields := entry.ContextMap()
	if got := fields["status"]; got != int64(http.StatusNotFound) {
		t.Errorf("status = %v, want 404", got)
	}
	if s := fmt.Sprint(fields); strings.Contains(s, "/nope") {
		t.Errorf("the access log carries the raw path: %s", s)
	}
}

func TestProbesLogAtDebug(t *testing.T) {
	logs := observeLogs(t)
	h := newRouter(keyring{})
	for _, path := range []string{"/healthz", "/readyz"} {
		get(t, h, path, "")
		entry := requestLine(t, logs, path)
		if entry.Level != zapcore.DebugLevel {
			t.Errorf("%s level = %v, want DEBUG", path, entry.Level)
		}
	}
}

func TestAccessLogPassesFlushedFramesToTheClient(t *testing.T) {
	release := make(chan struct{})
	h := chi.NewRouter()
	h.Use(requestID, accessLog, recoverer)
	h.Get("/stream", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush through the access log: %v", err)
		}
		<-release
		_, _ = io.WriteString(w, "second")
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/stream")
		got <- result{resp, err}
	}()

	var resp *http.Response
	select {
	case r := <-got:
		if r.err != nil {
			close(release)
			t.Fatalf("GET /stream: %v", r.err)
		}
		resp = r.resp
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("no response arrived before the handler returned; the access log did not pass Flush through")
	}
	defer func() { _ = resp.Body.Close() }()

	first := make([]byte, len("first"))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		close(release)
		t.Fatalf("read the flushed frame: %v", err)
	}
	if string(first) != "first" {
		t.Errorf("first frame = %q, want %q", first, "first")
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil || string(rest) != "second" {
		t.Errorf("rest of the body = %q, %v; want %q", rest, err, "second")
	}
}

func TestAPanicIsLoggedAtErrorAndAnsweredAs500(t *testing.T) {
	logs := observeLogs(t)
	h := chi.NewRouter()
	h.Use(requestID, accessLog, recoverer)
	h.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != "internal server error" {
		t.Errorf("body = %s, want the router's 500 error", rec.Body)
	}

	var panicEntry *observer.LoggedEntry
	for _, entry := range logs.All() {
		if entry.Message == "panic" {
			e := entry
			panicEntry = &e
		}
	}
	if panicEntry == nil {
		t.Fatal("no panic log entry")
	}
	if panicEntry.Level != zapcore.ErrorLevel {
		t.Errorf("panic level = %v, want ERROR", panicEntry.Level)
	}
	fields := panicEntry.ContextMap()
	if fields["panic"] != "kaboom" {
		t.Errorf("panic field = %v, want kaboom", fields["panic"])
	}
	if panicEntry.Stack == "" {
		t.Error("panic entry has no stacktrace")
	}
	if fields["request_id"] != rec.Header().Get("X-Request-Id") {
		t.Errorf("panic request_id = %v, want %q", fields["request_id"], rec.Header().Get("X-Request-Id"))
	}

	entry := requestLine(t, logs, "/boom")
	if got := entry.ContextMap()["status"]; got != int64(http.StatusInternalServerError) {
		t.Errorf("access log status = %v, want 500", got)
	}
	if entry.Level != zapcore.ErrorLevel {
		t.Errorf("access log level = %v, want ERROR", entry.Level)
	}
}
