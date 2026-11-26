package logger

import (
	"context"

	"go.uber.org/zap"

	logctx "github.com/phongnd2802/trelis/internal/logger/ctx"
)

// WithContext returns ctx carrying l, so code that has the ctx logs through
// Ctx instead of the global logger and keeps the request's fields.
func WithContext(ctx context.Context, l *zap.Logger) context.Context {
	return logctx.WithContext(ctx, l)
}

// Ctx returns the request-scoped logger, or the global logger when ctx
// carries none.
func Ctx(ctx context.Context) *zap.Logger {
	return logctx.Ctx(ctx)
}
