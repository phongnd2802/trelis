package logger

import (
	temporallog "go.temporal.io/sdk/log"
	"go.uber.org/zap"
)

// Temporal adapts zap for the Temporal SDK: keyvals become typed fields and
// the caller is the Temporal call site, not this adapter.
type Temporal struct {
	*zap.SugaredLogger
}

var (
	_ temporallog.Logger     = (*Temporal)(nil)
	_ temporallog.WithLogger = (*Temporal)(nil)
)

// NewTemporal builds the adapter Temporal clients log through.
func NewTemporal(l *zap.Logger) *Temporal {
	return &Temporal{l.WithOptions(zap.AddCallerSkip(1)).Sugar()}
}

// Debug logs at debug level.
func (t *Temporal) Debug(msg string, keyvals ...any) { t.Debugw(msg, keyvals...) }

// Info logs at info level.
func (t *Temporal) Info(msg string, keyvals ...any) { t.Infow(msg, keyvals...) }

// Warn logs at warn level.
func (t *Temporal) Warn(msg string, keyvals ...any) { t.Warnw(msg, keyvals...) }

// Error logs at error level.
func (t *Temporal) Error(msg string, keyvals ...any) { t.Errorw(msg, keyvals...) }

// With returns a logger that prepends keyvals, without mutating this one.
func (t *Temporal) With(keyvals ...any) temporallog.Logger {
	return &Temporal{t.SugaredLogger.With(keyvals...)}
}
