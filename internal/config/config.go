// Package config loads Trelis configuration from a YAML file, with
// environment variables overriding individual keys.
//
// A key's env name is TRELIS_ plus its YAML path in upper case with dots
// turned into underscores: server.shutdown_timeout → TRELIS_SERVER_SHUTDOWN_TIMEOUT.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const envPrefix = "TRELIS"

// minShutdownTimeout rejects unit-less values such as `5`, which would
// otherwise decode as 5ns and silently disable the drain.
const minShutdownTimeout = time.Second

// Config is the whole service configuration.
type Config struct {
	// Env is development or production; it picks the log defaults.
	Env      string   `mapstructure:"env"`
	Server   Server   `mapstructure:"server"`
	Postgres Postgres `mapstructure:"postgres"`
	Neo4j    Neo4j    `mapstructure:"neo4j"`
	Temporal Temporal `mapstructure:"temporal"`
	Storage  Storage  `mapstructure:"storage"`
	Models   Models   `mapstructure:"models"`
	Log      Log      `mapstructure:"log"`
}

// Server configures the HTTP listener.
type Server struct {
	// Addr is the host:port to listen on; ":8080" listens on every interface.
	Addr string `mapstructure:"addr"`
	// ShutdownTimeout bounds how long SIGTERM waits for in-flight requests.
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	// Swagger serves the Swagger UI and the generated spec at /swagger/,
	// open and unauthenticated. The code default is false; config/trelis.yaml
	// turns it on (and the Docker image bakes that file in), so production
	// must set TRELIS_SERVER_SWAGGER=false. Otherwise anyone who can reach
	// the port can read the API description.
	Swagger bool `mapstructure:"swagger"`
}

// defaults also registers every key with viper, which is what lets an env
// variable override a key the YAML file leaves out.
var defaults = map[string]any{
	"env": EnvDevelopment,

	"server.addr":             ":8080",
	"server.shutdown_timeout": 10 * time.Second,
	"server.swagger":          false,

	"postgres.host":     "localhost",
	"postgres.port":     5432,
	"postgres.dbname":   "trelis",
	"postgres.user":     "trelis",
	"postgres.password": "",
	"postgres.sslmode":  "disable",

	"neo4j.host":     "localhost",
	"neo4j.port":     7687,
	"neo4j.user":     "neo4j",
	"neo4j.password": "",

	"temporal.host":       "localhost",
	"temporal.port":       7233,
	"temporal.namespace":  "default",
	"temporal.task_queue": "trelis-ingest",

	"storage.max_file_size_mb":     50,
	"storage.s3.endpoint":          "",
	"storage.s3.region":            "us-east-1",
	"storage.s3.bucket":            "trelis",
	"storage.s3.access_key_id":     "",
	"storage.s3.secret_access_key": "",
	"storage.s3.force_path_style":  false,
	"storage.s3.create_bucket":     false,

	// The empty values mean "follow env"; the int keys keep their value in
	// both environments, and the booleans have no default at all so IsSet
	// can tell "not set" from "set to false".
	"log.level":             "",
	"log.format":            "",
	"log.file.path":         "",
	"log.file.max_size_mb":  100,
	"log.file.max_backups":  7,
	"log.file.max_age_days": 28,
}

var sslModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// Load reads the YAML file at path, applies env overrides and validates the
// result. Every error names the file or the offending key.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	for key, value := range defaults {
		v.SetDefault(key, value)
	}
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// The log block's booleans have no registered default, so without an
	// explicit binding an env-only value would not reach IsSet or Unmarshal.
	// Binding every log key also keeps the environment as the one place an
	// operator sets them.
	for _, key := range logKeys {
		if err := v.BindEnv(key); err != nil {
			return nil, fmt.Errorf("config: bind %s: %w", envName(key), err)
		}
	}

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	err := v.UnmarshalExact(&cfg)
	if err == nil {
		cfg.Log = resolveLog(v, cfg.Env)
		err = cfg.validate()
	}
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", sources(path), err)
	}
	return &cfg, nil
}

// envName is the environment variable that overrides key.
func envName(key string) string {
	return envPrefix + "_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// sources names where the values came from, so an error caused by an env
// override does not send the reader to the YAML file.
func sources(path string) string {
	var set []string
	for key := range defaults {
		if os.Getenv(envName(key)) != "" {
			set = append(set, envName(key))
		}
	}
	for _, key := range logKeys {
		if os.Getenv(envName(key)) != "" {
			set = append(set, envName(key))
		}
	}
	if len(set) == 0 {
		return path
	}
	slices.Sort(set)
	// Some log keys are registered defaults as well as bound env keys.
	set = slices.Compact(set)
	return fmt.Sprintf("%s with env overrides %s", path, strings.Join(set, ", "))
}

func (c *Config) validate() error {
	var errs []error
	if !slices.Contains(envs, c.Env) {
		errs = append(errs, fmt.Errorf("env must be one of %s, got %q", strings.Join(envs, ", "), c.Env))
	}
	if c.Server.Addr == "" {
		errs = append(errs, errors.New("server.addr is required"))
	} else if _, _, err := net.SplitHostPort(c.Server.Addr); err != nil {
		errs = append(errs, fmt.Errorf("server.addr %q must be host:port: %w", c.Server.Addr, err))
	}
	if c.Server.ShutdownTimeout < minShutdownTimeout {
		errs = append(errs, fmt.Errorf("server.shutdown_timeout must be at least %v (write a unit, e.g. 10s), got %v", minShutdownTimeout, c.Server.ShutdownTimeout))
	}

	required := func(key, value string) {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
	}
	port := func(key string, value int) {
		if value < 1 || value > 65535 {
			errs = append(errs, fmt.Errorf("%s must be between 1 and 65535, got %d", key, value))
		}
	}

	required("postgres.host", c.Postgres.Host)
	port("postgres.port", c.Postgres.Port)
	required("postgres.dbname", c.Postgres.DBName)
	required("postgres.user", c.Postgres.User)
	if !slices.Contains(sslModes, c.Postgres.SSLMode) {
		errs = append(errs, fmt.Errorf("postgres.sslmode must be one of %s, got %q", strings.Join(sslModes, ", "), c.Postgres.SSLMode))
	}

	required("neo4j.host", c.Neo4j.Host)
	port("neo4j.port", c.Neo4j.Port)
	required("neo4j.user", c.Neo4j.User)

	required("temporal.host", c.Temporal.Host)
	port("temporal.port", c.Temporal.Port)
	required("temporal.namespace", c.Temporal.Namespace)
	required("temporal.task_queue", c.Temporal.TaskQueue)

	if c.Storage.MaxFileSizeMB < 1 {
		errs = append(errs, fmt.Errorf("storage.max_file_size_mb must be at least 1, got %d", c.Storage.MaxFileSizeMB))
	}
	errs = append(errs, c.Storage.S3.validate()...)
	errs = append(errs, c.Models.validate()...)
	errs = append(errs, c.Log.validate()...)

	return errors.Join(errs...)
}
