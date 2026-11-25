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
	Server Server `mapstructure:"server"`
}

// Server configures the HTTP listener.
type Server struct {
	// Addr is the host:port to listen on; ":8080" listens on every interface.
	Addr string `mapstructure:"addr"`
	// ShutdownTimeout bounds how long SIGTERM waits for in-flight requests.
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

// defaults also registers every key with viper, which is what lets an env
// variable override a key the YAML file leaves out.
var defaults = map[string]any{
	"server.addr":             ":8080",
	"server.shutdown_timeout": 10 * time.Second,
}

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

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	err := v.UnmarshalExact(&cfg)
	if err == nil {
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
	if len(set) == 0 {
		return path
	}
	slices.Sort(set)
	return fmt.Sprintf("%s with env overrides %s", path, strings.Join(set, ", "))
}

func (c *Config) validate() error {
	var errs []error
	if c.Server.Addr == "" {
		errs = append(errs, errors.New("server.addr is required"))
	} else if _, _, err := net.SplitHostPort(c.Server.Addr); err != nil {
		errs = append(errs, fmt.Errorf("server.addr %q must be host:port: %w", c.Server.Addr, err))
	}
	if c.Server.ShutdownTimeout < minShutdownTimeout {
		errs = append(errs, fmt.Errorf("server.shutdown_timeout must be at least %v (write a unit, e.g. 10s), got %v", minShutdownTimeout, c.Server.ShutdownTimeout))
	}
	return errors.Join(errs...)
}
