package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/phongnd2802/trelis/internal/config"
)

// logConfig is a logger config writing to a file in the test's temp dir.
func logConfig(t *testing.T) config.Log {
	t.Helper()
	return config.Log{
		Level:     "info",
		Format:    "json",
		AddSource: false,
		File: config.LogFile{
			Enabled:    true,
			Path:       filepath.Join(t.TempDir(), "trelis.log"),
			MaxSizeMB:  100,
			MaxBackups: 7,
			MaxAgeDays: 28,
			Compress:   true,
		},
	}
}

func TestNewWritesJSONToTheFile(t *testing.T) {
	cfg := logConfig(t)
	var stdout bytes.Buffer
	l, closeLog, err := newLogger(cfg, &stdout)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	l.Info("hello", zap.String("k", "v"))
	if err := closeLog(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw, err := os.ReadFile(cfg.File.Path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &entry); err != nil {
		t.Fatalf("log file line %q is not JSON: %v", raw, err)
	}
	if entry["msg"] != "hello" || entry["k"] != "v" || entry["level"] != "info" {
		t.Errorf("file entry = %v, want msg hello, k v, level info", entry)
	}
	if ts, ok := entry["time"].(string); !ok || !strings.HasSuffix(ts, "Z") || !strings.HasPrefix(ts, "20") {
		t.Errorf("time = %v, want ISO8601 in UTC", entry["time"])
	}
}

func TestNewWithWriterUsesTheGivenWriter(t *testing.T) {
	cfg := logConfig(t)
	cfg.File.Enabled = false
	var out bytes.Buffer
	l, closeLog, err := NewWithWriter(cfg, &out)
	if err != nil {
		t.Fatalf("NewWithWriter: %v", err)
	}
	l.Info("hello")
	if err := closeLog(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Errorf("writer = %q, want the log line", out.String())
	}
}

func TestStdoutFormatIsConsoleOrJSON(t *testing.T) {
	cases := map[string]struct {
		format     string
		wantInOut  string
		wantNotOut string
	}{
		"console": {"console", "hello", `"msg"`},
		"json":    {"json", `"msg":"hello"`, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := logConfig(t)
			cfg.Format = tc.format
			cfg.File.Enabled = false
			var stdout bytes.Buffer
			l, closeLog, err := newLogger(cfg, &stdout)
			if err != nil {
				t.Fatalf("newLogger: %v", err)
			}
			l.Info("hello")
			if err := closeLog(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if !strings.Contains(stdout.String(), tc.wantInOut) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tc.wantInOut)
			}
			if tc.wantNotOut != "" && strings.Contains(stdout.String(), tc.wantNotOut) {
				t.Errorf("stdout = %q, want it not to contain %q", stdout.String(), tc.wantNotOut)
			}
		})
	}
}

func TestLevelFiltersTheStdoutCoreToo(t *testing.T) {
	cfg := logConfig(t)
	cfg.File.Enabled = false
	var stdout bytes.Buffer
	l, closeLog, err := newLogger(cfg, &stdout)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	l.Debug("hidden")
	l.Info("shown")
	_ = closeLog()

	if strings.Contains(stdout.String(), "hidden") {
		t.Errorf("stdout = %q, want the debug line dropped at level info", stdout.String())
	}
	if !strings.Contains(stdout.String(), "shown") {
		t.Errorf("stdout = %q, want the info line", stdout.String())
	}
}

func TestNewRejectsAnUnwritableLogPath(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := logConfig(t)
	cfg.File.Path = filepath.Join(blocker, "trelis.log")

	if _, _, err := newLogger(cfg, &bytes.Buffer{}); err == nil {
		t.Fatal("newLogger succeeded, want an error for a path under a regular file")
	}
}

func TestCtxFallsBackToTheGlobalLogger(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	if got := Ctx(context.Background()); got != zap.L() {
		t.Error("Ctx on a bare context did not return zap.L()")
	}

	own := zap.NewNop()
	ctx := WithContext(context.Background(), own)
	if got := Ctx(ctx); got != own {
		t.Error("Ctx did not return the logger WithContext put in the context")
	}
	_ = logs
}

func TestTemporalAdapterTurnsKeyvalsIntoFields(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	adapter := NewTemporal(zap.New(core))

	adapter.Info("hello", "k", "v", "n", 1)
	with := adapter.With("role", "chat")
	with.Warn("answer")

	entries := logs.All()
	if len(entries) != 2 {
		t.Fatalf("logger saw %d entries, want 2", len(entries))
	}
	if entries[0].Message != "hello" || entries[0].Level != zapcore.InfoLevel {
		t.Errorf("entry = %+v, want hello at info", entries[0])
	}
	if got := entries[0].ContextMap(); got["k"] != "v" || got["n"] != int64(1) {
		t.Errorf("fields = %v, want k=v and n=1", got)
	}
	if got := entries[1].ContextMap(); got["role"] != "chat" {
		t.Errorf("With fields = %v, want role=chat", got)
	}
}

func TestTemporalAdapterSkipsItsOwnFrame(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	adapter := NewTemporal(zap.New(core, zap.AddCaller()))

	adapter.Info("hello")

	if caller := logs.All()[0].Caller; !strings.HasSuffix(caller.File, "logger_test.go") {
		t.Errorf("caller = %s:%d, want the caller's frame", caller.File, caller.Line)
	}
}
