package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Serve answers requests on ln until ctx is cancelled, then stops accepting
// new connections and waits up to shutdownTimeout for in-flight requests to
// finish. It returns nil after a clean drain, the shutdown error (wrapping
// context.DeadlineExceeded) when the drain times out, or the listener error
// if serving fails first.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler, shutdownTimeout time.Duration) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("router: serve: %w", err)
	case <-ctx.Done():
	}

	// The parent ctx is already cancelled, so the drain gets a fresh deadline.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("router: shutdown after %v: %w", shutdownTimeout, err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("router: serve: %w", err)
	}
	return nil
}
