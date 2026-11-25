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
