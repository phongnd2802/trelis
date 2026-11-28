// Package shutdown carries a server's shutting-down notice through request
// contexts. Shutdown does not cancel in-flight request contexts, so an
// ordinary request finishes draining; a long-lived stream selects on this
// notice to end promptly instead of holding the drain until its timeout.
package shutdown

import "context"

type signalKey struct{}

// WithSignal returns ctx carrying done, the channel a server closes when it
// begins shutting down.
func WithSignal(ctx context.Context, done <-chan struct{}) context.Context {
	return context.WithValue(ctx, signalKey{}, done)
}

// Signal returns the channel in ctx that the server closes when shutdown
// begins. It is nil when ctx carries none; a receive from a nil channel blocks
// forever, so a select case on it is inert outside a server.
func Signal(ctx context.Context) <-chan struct{} {
	done, _ := ctx.Value(signalKey{}).(<-chan struct{})
	return done
}
