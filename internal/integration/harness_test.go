//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcneo4j "github.com/testcontainers/testcontainers-go/modules/neo4j"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/phongnd2802/trelis/internal/fakeprovider"
)

// The images compose.dev.yaml runs; keep them in step.
const (
	postgresImage = "paradedb/paradedb:0.27.1-pg18"
	neo4jImage    = "neo4j:5.26.34-community"
)

const (
	pgUser, pgPassword = "trelis", "trelis"
	neo4jPassword      = "trelis-test"
	// bootTimeout bounds how long a Trelis process may take to migrate and
	// start listening.
	bootTimeout = 30 * time.Second
)

// deps is what TestMain starts once for the whole package.
var deps struct {
	binary string
	pgHost string
	pgPort string
	neo4j  hostPort
}

type hostPort struct{ host, port string }

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// run builds Trelis and starts the containers, runs the tests, then tears
// everything down.
func run(m *testing.M) (int, error) {
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "trelis-integration-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	deps.binary = filepath.Join(dir, "trelis")
	build := exec.Command("go", "build", "-tags", buildTags, "-o", deps.binary, "github.com/phongnd2802/trelis/cmd/server")
	if out, err := build.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("build trelis: %w\n%s", err, out)
	}

	var (
		wg              sync.WaitGroup
		pg              *tcpostgres.PostgresContainer
		graph           *tcneo4j.Neo4jContainer
		pgErr, neo4jErr error
	)
	wg.Go(func() {
		pg, pgErr = tcpostgres.Run(ctx, postgresImage,
			tcpostgres.WithDatabase("trelis"),
			tcpostgres.WithUsername(pgUser),
			tcpostgres.WithPassword(pgPassword),
			tcpostgres.BasicWaitStrategies(),
		)
	})
	wg.Go(func() {
		graph, neo4jErr = tcneo4j.Run(ctx, neo4jImage, tcneo4j.WithAdminPassword(neo4jPassword))
	})
	wg.Wait()
	defer func() {
		_ = testcontainers.TerminateContainer(pg)
		_ = testcontainers.TerminateContainer(graph)
	}()
	if pgErr != nil {
		return 0, fmt.Errorf("start postgres: %w", pgErr)
	}
	if neo4jErr != nil {
		return 0, fmt.Errorf("start neo4j: %w", neo4jErr)
	}

	if deps.pgHost, deps.pgPort, err = endpoint(ctx, pg, "5432/tcp"); err != nil {
		return 0, err
	}
	if deps.neo4j.host, deps.neo4j.port, err = endpoint(ctx, graph, "7687/tcp"); err != nil {
		return 0, err
	}
	return m.Run(), nil
}

// endpoint is where port of c is reachable from the host.
func endpoint(ctx context.Context, c testcontainers.Container, port string) (string, string, error) {
	host, err := c.Host(ctx)
	if err != nil {
		return "", "", err
	}
	mapped, err := c.MappedPort(ctx, port)
	if err != nil {
		return "", "", err
	}
	return host, mapped.Port(), nil
}

// trelis is one running Trelis process with its own Postgres database and
// its own fake model provider.
type trelis struct {
	// url is the base URL of its HTTP API, e.g. http://127.0.0.1:41234.
	url string
	// provider is the fake model provider this Trelis is meant to call. No
	// model role is configurable yet; when roles land, point them at
	// provider.URL() in config below.
	provider *fakeprovider.Provider
	config   string
}

// listening matches the log line Trelis prints once it accepts requests.
var listening = regexp.MustCompile(`trelis listening addr=(\S+)`)

// start boots Trelis on a fresh, empty database and stops it when the test
// ends. Neo4j is shared by every test in the package.
func start(t *testing.T) *trelis {
	t.Helper()
	tr := &trelis{provider: fakeprovider.New(t)}
	tr.config = writeConfig(t, freshDatabase(t))

	cmd := exec.Command(deps.binary, "-config", tr.config)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	cmd.Stdout = &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start trelis: %v", err)
	}
	exited := make(chan error, 1)
	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			_, _ = fmt.Fprintln(&logs, line)
			if m := listening.FindStringSubmatch(line); m != nil {
				addr <- m[1]
			}
		}
		_, _ = io.Copy(io.Discard, stderr)
		exited <- cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("trelis exited with %v", err)
			}
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			t.Errorf("trelis did not stop on SIGTERM")
		}
		if t.Failed() {
			t.Logf("trelis output:\n%s", logs.String())
		}
	})

	select {
	case a := <-addr:
		tr.url = "http://" + a
	case err := <-exited:
		exited <- err // for the cleanup
		t.Fatalf("trelis exited before listening: %v\n%s", err, logs.String())
	case <-time.After(bootTimeout):
		t.Fatalf("trelis not listening after %v:\n%s", bootTimeout, logs.String())
	}
	return tr
}

// freshDatabase creates an empty database on the shared Postgres, dropped
// when the test ends, and returns its name.
func freshDatabase(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s:%s/trelis?sslmode=disable", pgUser, pgPassword, deps.pgHost, deps.pgPort))
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	name := fmt.Sprintf("trelis_it_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = conn.Close(ctx)
	})
	return name
}

// writeConfig writes a Trelis config file for dbname and returns its path.
// Temporal is left unreachable: the client connects lazily and nothing
// under test uses it yet.
func writeConfig(t *testing.T, dbname string) string {
	t.Helper()
	cfg := fmt.Sprintf(`server:
  addr: "127.0.0.1:0"
  shutdown_timeout: 5s
postgres:
  host: %q
  port: %s
  dbname: %q
  user: %q
  password: %q
  sslmode: disable
neo4j:
  host: %q
  port: %s
  user: neo4j
  password: %q
temporal:
  host: 127.0.0.1
  port: 1
  namespace: default
`, deps.pgHost, deps.pgPort, dbname, pgUser, pgPassword, deps.neo4j.host, deps.neo4j.port, neo4jPassword)
	path := filepath.Join(t.TempDir(), "trelis.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// admin runs `server admin <args>` against this Trelis's database, as an
// operator would, and returns its trimmed stdout.
func (tr *trelis) admin(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(deps.binary, append([]string{"-config", tr.config, "admin"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("admin %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// get sends GET path with key as the bearer token (none when empty). The
// body is closed when the test ends.
func (tr *trelis) get(t *testing.T, path, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, tr.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// syncBuffer is a bytes.Buffer safe for concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
