package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trelis.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validYAML = `
server:
  addr: ":9090"
  shutdown_timeout: 5s
`

func TestLoadReadsYAML(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":9090" {
		t.Errorf("Server.Addr = %q, want %q", cfg.Server.Addr, ":9090")
	}
	if cfg.Server.ShutdownTimeout != 5*time.Second {
		t.Errorf("Server.ShutdownTimeout = %v, want 5s", cfg.Server.ShutdownTimeout)
	}
}

func TestLoadEnvOverridesIndividualKeys(t *testing.T) {
	t.Setenv("TRELIS_SERVER_ADDR", "127.0.0.1:7000")

	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:7000" {
		t.Errorf("Server.Addr = %q, want the env value 127.0.0.1:7000", cfg.Server.Addr)
	}
	if cfg.Server.ShutdownTimeout != 5*time.Second {
		t.Errorf("Server.ShutdownTimeout = %v, want 5s from YAML", cfg.Server.ShutdownTimeout)
	}
}

func TestLoadEnvOverridesKeyMissingFromYAML(t *testing.T) {
	t.Setenv("TRELIS_SERVER_SHUTDOWN_TIMEOUT", "30s")

	cfg, err := Load(writeConfig(t, "server:\n  addr: \":8080\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ShutdownTimeout != 30*time.Second {
		t.Errorf("Server.ShutdownTimeout = %v, want 30s from env", cfg.Server.ShutdownTimeout)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		env     map[string]string
		wantMsg string
	}{
		{"unknown key", validYAML + "  port: 1\n", nil, "port"},
		{"empty addr", "server:\n  addr: \"\"\n  shutdown_timeout: 5s\n", nil, "server.addr"},
		{"addr without port", "server:\n  addr: \"localhost\"\n  shutdown_timeout: 5s\n", nil, "server.addr"},
		{"zero shutdown timeout", "server:\n  addr: \":8080\"\n  shutdown_timeout: 0s\n", nil, "server.shutdown_timeout"},
		{"shutdown timeout without unit", "server:\n  addr: \":8080\"\n  shutdown_timeout: 5\n", nil, "server.shutdown_timeout"},
		{"bad env addr names the env var", validYAML, map[string]string{"TRELIS_SERVER_ADDR": "localhost"}, "TRELIS_SERVER_ADDR"},
		{"bad duration", "server:\n  addr: \":8080\"\n  shutdown_timeout: soon\n", nil, "shutdown_timeout"},
		{"bad env duration names the env var", validYAML, map[string]string{"TRELIS_SERVER_SHUTDOWN_TIMEOUT": "soon"}, "TRELIS_SERVER_SHUTDOWN_TIMEOUT"},
		{"malformed yaml", "server: [\n", nil, "trelis.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load(writeConfig(t, tc.yaml))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestLoadMissingFileNamesThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.yaml")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("Load(missing) err = %v, want it to name %s", err, path)
	}
}

const depsYAML = `
server:
  addr: ":8080"
  shutdown_timeout: 5s
postgres:
  host: db
  port: 5433
  dbname: trelis
  user: trelis
  password: from-yaml
neo4j:
  host: graph
  port: 7688
  user: neo4j
  password: graphpw
temporal:
  host: temporal
  port: 7234
  namespace: trelis
`

func TestLoadDependencies(t *testing.T) {
	t.Setenv("TRELIS_POSTGRES_PASSWORD", "s3cr/t @x")

	cfg, err := Load(writeConfig(t, depsYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Postgres.DSN(), "postgres://trelis:s3cr%2Ft%20%40x@db:5433/trelis?sslmode=disable"; got != want {
		t.Errorf("Postgres.DSN() = %q, want %q", got, want)
	}
	if got, want := cfg.Neo4j.URI(), "bolt://graph:7688"; got != want {
		t.Errorf("Neo4j.URI() = %q, want %q", got, want)
	}
	if cfg.Neo4j.User != "neo4j" || cfg.Neo4j.Password != "graphpw" {
		t.Errorf("Neo4j credentials = %q/%q, want neo4j/graphpw", cfg.Neo4j.User, cfg.Neo4j.Password)
	}
	if got, want := cfg.Temporal.HostPort(), "temporal:7234"; got != want {
		t.Errorf("Temporal.HostPort() = %q, want %q", got, want)
	}
	if cfg.Temporal.Namespace != "trelis" {
		t.Errorf("Temporal.Namespace = %q, want trelis", cfg.Temporal.Namespace)
	}
}

func TestLoadDependencyDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Postgres.DSN(), "postgres://trelis@localhost:5432/trelis?sslmode=disable"; got != want {
		t.Errorf("default Postgres.DSN() = %q, want %q", got, want)
	}
	if got, want := cfg.Neo4j.URI(), "bolt://localhost:7687"; got != want {
		t.Errorf("default Neo4j.URI() = %q, want %q", got, want)
	}
	if got, want := cfg.Temporal.HostPort(), "localhost:7233"; got != want {
		t.Errorf("default Temporal.HostPort() = %q, want %q", got, want)
	}
}

func TestLoadRejectsInvalidDependencies(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantMsg string
	}{
		{"empty postgres host", map[string]string{"TRELIS_POSTGRES_HOST": " "}, "postgres.host"},
		{"postgres port zero", map[string]string{"TRELIS_POSTGRES_PORT": "0"}, "postgres.port"},
		{"postgres port too big", map[string]string{"TRELIS_POSTGRES_PORT": "70000"}, "postgres.port"},
		{"postgres port not a number", map[string]string{"TRELIS_POSTGRES_PORT": "abc"}, "postgres.port"},
		{"empty postgres dbname", map[string]string{"TRELIS_POSTGRES_DBNAME": " "}, "postgres.dbname"},
		{"empty postgres user", map[string]string{"TRELIS_POSTGRES_USER": " "}, "postgres.user"},
		{"bad sslmode", map[string]string{"TRELIS_POSTGRES_SSLMODE": "maybe"}, "postgres.sslmode"},
		{"empty neo4j host", map[string]string{"TRELIS_NEO4J_HOST": " "}, "neo4j.host"},
		{"neo4j port too big", map[string]string{"TRELIS_NEO4J_PORT": "99999"}, "neo4j.port"},
		{"empty temporal host", map[string]string{"TRELIS_TEMPORAL_HOST": " "}, "temporal.host"},
		{"temporal port zero", map[string]string{"TRELIS_TEMPORAL_PORT": "0"}, "temporal.port"},
		{"empty temporal namespace", map[string]string{"TRELIS_TEMPORAL_NAMESPACE": " "}, "temporal.namespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load(writeConfig(t, depsYAML))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tc.wantMsg)
			}
		})
	}
}
