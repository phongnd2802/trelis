package router

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/phongnd2802/trelis/internal/shutdown"
)

// slowHandler signals when a request arrives and answers only once release closes.
func slowHandler(entered chan<- struct{}, release <-chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = io.WriteString(w, "done")
	})
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestServeDrainsInFlightRequestsOnShutdown(t *testing.T) {
	ln := listen(t)
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, slowHandler(entered, release), 5*time.Second) }()

	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		got <- result{string(b), err}
	}()

	<-entered
	cancel() // what SIGTERM does in main

	select {
	case err := <-served:
		t.Fatalf("Serve returned %v while a request was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if r := <-got; r.err != nil || r.body != "done" {
		t.Fatalf("in-flight request = (%q, %v), want (\"done\", nil)", r.body, r.err)
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve = %v, want nil after a clean drain", err)
	}
}

func TestServeGivesUpAfterShutdownTimeout(t *testing.T) {
	ln := listen(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, slowHandler(entered, release), 50*time.Millisecond) }()

	go func() {
		if resp, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	cancel()

	select {
	case err := <-served:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Serve = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the shutdown timeout")
	}
}

func TestServeReturnsListenerErrors(t *testing.T) {
	ln := listen(t)
	_ = ln.Close()

	err := Serve(context.Background(), ln, http.NotFoundHandler(), time.Second)
	if err == nil {
		t.Fatal("Serve on a closed listener returned nil, want an error")
	}
}

func TestShutdownLeavesAnInFlightRequestItsContext(t *testing.T) {
	ln := listen(t)
	entered, release := make(chan struct{}), make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		if err := r.Context().Err(); err != nil {
			http.Error(w, "request context cancelled: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "done")
	})

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, h, 5*time.Second) }()

	type result struct {
		body   string
		status int
		err    error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		got <- result{string(b), resp.StatusCode, err}
	}()

	<-entered
	cancel() // what SIGTERM does in main

	select {
	case err := <-served:
		t.Fatalf("Serve returned %v while an ordinary request was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if r := <-got; r.err != nil || r.status != http.StatusOK || r.body != "done" {
		t.Fatalf("in-flight request = (%d, %q, %v), want (200, \"done\", nil); shutdown must not cancel it",
			r.status, r.body, r.err)
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve = %v, want nil after a clean drain", err)
	}
}

func TestShutdownEndsAnOpenStreamPromptly(t *testing.T) {
	ln := listen(t)
	opened, ended := make(chan struct{}), make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush the stream: %v", err)
		}
		close(opened)
		select {
		case <-r.Context().Done():
		case <-shutdown.Signal(r.Context()):
		}
		close(ended)
	})

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, h, 5*time.Second) }()

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("the stream did not open")
	}
	cancel() // what SIGTERM does in main

	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not signal the open stream")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve = %v, want nil after the stream ended and the drain finished", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve waited on the open stream instead of ending it at shutdown")
	}
	<-clientDone
}
