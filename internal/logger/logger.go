// Package logger builds Trelis's zap logger: every record goes to stdout
// (console in development, JSON in production) and to a JSON file rotated by
// lumberjack. Request-scoped fields travel in the context; see WithContext and
// Ctx.
package logger

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/phongnd2802/trelis/internal/config"
)

// New builds the logger for cfg and returns it with a func that flushes both
// cores and closes the rotated file. A log file that cannot be created is an
// error here, at startup, not a silently dropped record later.
func New(cfg config.Log) (*zap.Logger, func() error, error) {
	return newLogger(cfg, os.Stdout)
}

// NewWithWriter is New with the stdout core writing to w. The server uses New;
// an admin subcommand passes os.Stderr so its result on stdout stays parsable.
// The file core is unaffected.
func NewWithWriter(cfg config.Log, w io.Writer) (*zap.Logger, func() error, error) {
	return newLogger(cfg, w)
}

// newLogger is New with the stdout writer injected, so tests can capture it.
func newLogger(cfg config.Log, stdout io.Writer) (*zap.Logger, func() error, error) {
	level, err := zapcore.ParseLevel(cfg.Level)
	if err != nil {
		return nil, nil, fmt.Errorf("logger: level %q: %w", cfg.Level, err)
	}

	encCfg := encoderConfig()
	stdoutEnc := zapcore.NewJSONEncoder(encCfg)
	if cfg.Format == "console" {
		stdoutEnc = zapcore.NewConsoleEncoder(encCfg)
	}
	stdoutWS := zapcore.AddSync(stdout)
	cores := []zapcore.Core{zapcore.NewCore(stdoutEnc, stdoutWS, level)}

	var (
		file   *lumberjack.Logger
		fileWS zapcore.WriteSyncer
	)
	if cfg.File.Enabled {
		if err := os.MkdirAll(filepath.Dir(cfg.File.Path), 0o755); err != nil {
			return nil, nil, fmt.Errorf("logger: create log directory for %s: %w", cfg.File.Path, err)
		}
		// Open and stat once up front, so a path that cannot be written fails
		// the start instead of every record failing inside lumberjack.
		f, err := os.OpenFile(cfg.File.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("logger: open log file %s: %w", cfg.File.Path, err)
		}
		_, statErr := f.Stat()
		closeErr := f.Close()
		if statErr != nil {
			return nil, nil, fmt.Errorf("logger: stat log file %s: %w", cfg.File.Path, statErr)
		}
		if closeErr != nil {
			return nil, nil, fmt.Errorf("logger: close log file %s: %w", cfg.File.Path, closeErr)
		}

		file = &lumberjack.Logger{
			Filename:   cfg.File.Path,
			MaxSize:    cfg.File.MaxSizeMB,
			MaxBackups: cfg.File.MaxBackups,
			MaxAge:     cfg.File.MaxAgeDays,
			Compress:   cfg.File.Compress,
		}
		fileWS = zapcore.AddSync(file)
		cores = append(cores, zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), fileWS, level))
	}

	opts := []zap.Option{zap.AddStacktrace(zapcore.ErrorLevel)}
	if cfg.AddSource {
		opts = append(opts, zap.AddCaller())
	}
	l := zap.New(zapcore.NewTee(cores...), opts...)

	closeLog := func() error {
		var errs []error
		// Syncing a terminal returns EINVAL/ENOTTY on Linux; there is nothing
		// to flush and the call is expected to fail there.
		if err := stdoutWS.Sync(); err != nil && !isTerminalSyncError(err) {
			errs = append(errs, fmt.Errorf("sync stdout: %w", err))
		}
		if fileWS != nil {
			if err := fileWS.Sync(); err != nil {
				errs = append(errs, fmt.Errorf("sync log file: %w", err))
			}
			if err := file.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close log file: %w", err))
			}
		}
		return errors.Join(errs...)
	}
	return l, closeLog, nil
}

// encoderConfig is shared by both encoders: time key "time", ISO8601 in UTC.
// Stacktraces are automatic at ERROR and up, under zap's "stacktrace" key.
func encoderConfig() zapcore.EncoderConfig {
	return zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     encodeUTCISO8601,
		EncodeDuration: zapcore.MillisDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}
}

// encodeUTCISO8601 writes the timestamp in UTC, whatever the host's zone.
func encodeUTCISO8601(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString(t.UTC().Format("2006-01-02T15:04:05.000Z0700"))
}

func isTerminalSyncError(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTTY)
}
