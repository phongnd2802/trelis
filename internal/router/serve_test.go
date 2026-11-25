package router

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
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
