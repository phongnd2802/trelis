package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/phongnd2802/trelis/internal/shutdown"
)

// Serve answers requests on ln until ctx is cancelled, then stops accepting
// new connections and waits up to shutdownTimeout for in-flight requests to
// finish. Shutdown does not cancel an in-flight request's context, so an
// upload or answer drains normally; the request context carries a separate
// shutdown notice that a long-lived stream selects on to end promptly. It
// returns nil after a clean drain, the shutdown error (wrapping
// context.DeadlineExceeded) when the drain times out, or the listener error if
// serving fails first.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler, shutdownTimeout time.Duration) error {
	shuttingDown := make(chan struct{})
	var shutdownOnce sync.Once
	requestBase := shutdown.WithSignal(context.Background(), shuttingDown)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// Request contexts must not derive from ctx: it is cancelled when
		// shutdown starts, which would abort an ordinary in-flight request
		// instead of draining it. They carry the shutdown notice instead, so a
		// stream such as GET /documents/{id}/events can end without cancelling
		// the rest.
		BaseContext: func(net.Listener) context.Context { return requestBase },
	}
	srv.RegisterOnShutdown(func() {
		shutdownOnce.Do(func() { close(shuttingDown) })
	})

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
