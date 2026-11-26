// Package logctx carries the request-scoped zap logger in a context. It is a
// leaf so packages the logger itself depends on (config imports models/api)
// can read the logger without importing internal/logger, which would cycle.
package logctx

import (
	"context"

	"go.uber.org/zap"
)

type key struct{}

// WithContext returns ctx carrying l, so code that has the ctx logs through
// Ctx and keeps the request's fields.
func WithContext(ctx context.Context, l *zap.Logger) context.Context {
	return context.WithValue(ctx, key{}, l)
}

// Ctx returns the request-scoped logger, or the global logger when ctx
// carries none.
func Ctx(ctx context.Context) *zap.Logger {
	if l, ok := ctx.Value(key{}).(*zap.Logger); ok && l != nil {
		return l
	}
	return zap.L()
}
