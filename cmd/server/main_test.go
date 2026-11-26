package main

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestLogExit checks the decision the run defer makes: admin failures and a
// clean server exit stay silent, and a server failure logs once without the
// stacktrace that would only show the defer.
func TestLogExit(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core, zap.AddStacktrace(zapcore.ErrorLevel)))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	ctx := context.Background()
	logExit(ctx, true, errors.New("auth: not found"))
	logExit(ctx, false, nil)
	if got := logs.Len(); got != 0 {
		t.Fatalf("logged %d entries for an admin failure or a clean exit, want 0", got)
	}

	failure := errors.New("db: ping")
	logExit(ctx, false, failure)
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("logged %d entries for a server failure, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Message != "trelis exited with an error" || entry.Level != zapcore.ErrorLevel {
		t.Errorf("entry = %q at %v, want the ERROR exit line", entry.Message, entry.Level)
	}
	if entry.Stack != "" {
		t.Error("server exit error carries a stacktrace")
	}
	if got := entry.ContextMap()["error"]; got != failure.Error() {
		t.Errorf("error field = %v, want %q", got, failure)
	}
}
