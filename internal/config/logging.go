package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/viper"
)

// The two environments logging defaults depend on.
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
)

var (
	envs       = []string{EnvDevelopment, EnvProduction}
	logLevels  = []string{"debug", "info", "warn", "error"}
	logFormats = []string{"console", "json"}
)

// logKeys are the keys of the log block. The booleans are bound to the
// environment without a registered default, so IsSet can tell "not set" from
// "the key set false"; the other keys get their default from logDefaults.
var logKeys = []string{
	"log.level",
	"log.format",
	"log.add_source",
	"log.file.enabled",
	"log.file.path",
	"log.file.max_size_mb",
	"log.file.max_backups",
	"log.file.max_age_days",
	"log.file.compress",
}

// Log is the resolved logging configuration: env has already filled in every
// key the file and the environment left out, so readers never need to know
// about env.
type Log struct {
	// Level is debug, info, warn or error.
	Level string `mapstructure:"level"`
	// Format is console or json, for stdout; the file is always JSON.
	Format string `mapstructure:"format"`
	// AddSource adds the caller's file and line to every record.
	AddSource bool    `mapstructure:"add_source"`
	File      LogFile `mapstructure:"file"`
}

// LogFile is the rotated file beside stdout.
type LogFile struct {
	Enabled bool `mapstructure:"enabled"`
	// Path is the file's full path; its directory is created on start.
	Path string `mapstructure:"path"`
	// MaxSizeMB is when lumberjack rotates the file.
	MaxSizeMB int `mapstructure:"max_size_mb"`
	// MaxBackups is how many rotated files to keep, MaxAgeDays how long.
	MaxBackups int  `mapstructure:"max_backups"`
	MaxAgeDays int  `mapstructure:"max_age_days"`
	Compress   bool `mapstructure:"compress"`
}

// logDefaults is the table from ADR-0008: env picks the level, stdout format,
// caller and file path; the rotation settings are the same in both.
func logDefaults(env string) Log {
	file := LogFile{Enabled: true, MaxSizeMB: 100, MaxBackups: 7, MaxAgeDays: 28, Compress: true}
	if env == EnvProduction {
		file.Path = "/var/log/trelis/trelis.log"
		return Log{Level: "info", Format: "json", AddSource: false, File: file}
	}
	file.Path = "logs/trelis.log"
	return Log{Level: "debug", Format: "console", AddSource: true, File: file}
}

// resolveLog fills the log block from the env defaults, then from whichever
// keys the file or the environment set. An empty level, format or path means
// "follow env", as config/trelis.yaml documents.
func resolveLog(v *viper.Viper, env string) Log {
	out := logDefaults(env)
	if level := v.GetString("log.level"); level != "" {
		out.Level = level
	}
	if format := v.GetString("log.format"); format != "" {
		out.Format = format
	}
	if v.IsSet("log.add_source") {
		out.AddSource = v.GetBool("log.add_source")
	}
	if path := v.GetString("log.file.path"); path != "" {
		out.File.Path = path
	}
	if v.IsSet("log.file.enabled") {
		out.File.Enabled = v.GetBool("log.file.enabled")
	}
	out.File.MaxSizeMB = v.GetInt("log.file.max_size_mb")
	out.File.MaxBackups = v.GetInt("log.file.max_backups")
	out.File.MaxAgeDays = v.GetInt("log.file.max_age_days")
	if v.IsSet("log.file.compress") {
		out.File.Compress = v.GetBool("log.file.compress")
	}
	return out
}

func (l Log) validate() []error {
	var errs []error
	if !slices.Contains(logLevels, l.Level) {
		errs = append(errs, fmt.Errorf("log.level must be one of %s, got %q",
			strings.Join(logLevels, ", "), l.Level))
	}
	if !slices.Contains(logFormats, l.Format) {
		errs = append(errs, fmt.Errorf("log.format must be one of %s, got %q",
			strings.Join(logFormats, ", "), l.Format))
	}
	if l.File.Enabled && strings.TrimSpace(l.File.Path) == "" {
		errs = append(errs, fmt.Errorf("log.file.path is required when log.file.enabled is true"))
	}
	if l.File.MaxSizeMB < 1 {
		errs = append(errs, fmt.Errorf("log.file.max_size_mb must be at least 1, got %d", l.File.MaxSizeMB))
	}
	if l.File.MaxBackups < 0 {
		errs = append(errs, fmt.Errorf("log.file.max_backups must not be negative, got %d", l.File.MaxBackups))
	}
	if l.File.MaxAgeDays < 0 {
		errs = append(errs, fmt.Errorf("log.file.max_age_days must not be negative, got %d", l.File.MaxAgeDays))
	}
	return errs
}
