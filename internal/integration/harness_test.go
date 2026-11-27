//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	tcneo4j "github.com/testcontainers/testcontainers-go/modules/neo4j"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.temporal.io/sdk/testsuite"

	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

// The images compose.dev.yaml runs; keep them in step.
const (
	postgresImage = "paradedb/paradedb:0.27.1-pg18"
	neo4jImage    = "neo4j:5.26.34-community"
	// silo is an S3-compatible fork of MinIO, so the MinIO module runs it.
	siloImage = "docker.io/pgsty/silo:RELEASE.2026-11-12T00-00-00Z"
)

const (
	pgUser, pgPassword = "trelis", "trelis"
	neo4jPassword      = "trelis-test"
	s3User, s3Password = "trelis", "trelis-test-secret"
	// embeddingModel is the model name Trelis is configured with; the fake
	// provider echoes whatever name a role sends.
	embeddingModel = "test-embedding"
	// bootTimeout bounds how long a Trelis process may take to migrate and
	// start listening.
	bootTimeout = 30 * time.Second
)

// deps is what TestMain starts once for the whole package.
var deps struct {
	binary string
	config string
	pg     hostPort
	neo4j  hostPort
	// temporal is the shared Temporal dev server Trelis starts workflows on.
	temporal hostPort
	// s3 is the base URL of the shared silo server, e.g. http://127.0.0.1:41234.
	s3 string
}

// hostPort is where a container port is reachable from the host.
type hostPort struct{ host, port string }

func TestMain(m *testing.M) {
	code, err := runSuite(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// runSuite builds Trelis and starts the containers, runs the tests, then
// tears everything down.
func runSuite(m *testing.M) (int, error) {
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "trelis-integration-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if deps.binary, err = buildBinary(dir); err != nil {
		return 0, err
	}
	// Every key comes from TRELIS_* env (see trelisEnv), so the config file
	// is empty and a change to config/trelis.yaml cannot reach the tests.
	deps.config = filepath.Join(dir, "trelis.yaml")
	if err := os.WriteFile(deps.config, []byte("# all keys from TRELIS_* env\n"), 0o600); err != nil {
		return 0, err
	}

	var (
		wg                           sync.WaitGroup
		pg                           *tcpostgres.PostgresContainer
		graph                        *tcneo4j.Neo4jContainer
		silo                         *tcminio.MinioContainer
		devServer                    *testsuite.DevServer
		pgErr, neo4jErr, s3Err, tErr error
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
	wg.Go(func() {
		silo, s3Err = tcminio.Run(ctx, siloImage, tcminio.WithUsername(s3User), tcminio.WithPassword(s3Password))
	})
	wg.Go(func() {
		// The CLI-based dev server downloads once into the temp directory and
		// serves every test in the package from there.
		devServer, tErr = testsuite.StartDevServer(ctx, testsuite.DevServerOptions{})
	})
	wg.Wait()
	defer func() {
		_ = testcontainers.TerminateContainer(pg)
		_ = testcontainers.TerminateContainer(graph)
		_ = testcontainers.TerminateContainer(silo)
		if devServer != nil {
			_ = devServer.Stop()
		}
	}()
	if pgErr != nil {
		return 0, fmt.Errorf("start postgres: %w", pgErr)
	}
	if neo4jErr != nil {
		return 0, fmt.Errorf("start neo4j: %w", neo4jErr)
	}
	if s3Err != nil {
		return 0, fmt.Errorf("start silo: %w", s3Err)
	}
	if tErr != nil {
		return 0, fmt.Errorf("start temporal dev server: %w", tErr)
	}

	if deps.pg, err = endpoint(ctx, pg, "5432/tcp"); err != nil {
		return 0, err
	}
	if deps.neo4j, err = endpoint(ctx, graph, "7687/tcp"); err != nil {
		return 0, err
	}
	s3, err := endpoint(ctx, silo, "9000/tcp")
	if err != nil {
		return 0, err
	}
	deps.s3 = "http://" + net.JoinHostPort(s3.host, s3.port)
	host, port, err := net.SplitHostPort(devServer.FrontendHostPort())
	if err != nil {
		return 0, fmt.Errorf("temporal dev server address: %w", err)
	}
	deps.temporal = hostPort{host: host, port: port}
	return m.Run(), nil
}

// buildBinary builds the Trelis server into dir, with the same tags as this
// test binary, and returns its path. Built once per package, not per test.
//
// Adapted from Omnisc/WeKnora cli/acceptance/e2e/e2e_test.go,
// Copyright (C) 2026 Tencent, MIT license; see NOTICE.
func buildBinary(dir string) (string, error) {
	out := filepath.Join(dir, "trelis")
	// Repo layout: this test sits at internal/integration/, so the module
	// root is two levels up.
	cmd := exec.Command("go", "build", "-tags", buildTags, "-o", out, "./cmd/server")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build trelis: %w", err)
	}
	return out, nil
}

// endpoint is where port of c is reachable from the host.
func endpoint(ctx context.Context, c testcontainers.Container, port string) (hostPort, error) {
	host, err := c.Host(ctx)
	if err != nil {
		return hostPort{}, err
	}
	mapped, err := c.MappedPort(ctx, port)
	if err != nil {
		return hostPort{}, err
	}
	return hostPort{host: host, port: mapped.Port()}, nil
}

// trelis is one running Trelis process with its own Postgres database and
// its own fake model provider.
type trelis struct {
	// url is the base URL of its HTTP API, e.g. http://127.0.0.1:41234.
	url string
	// provider is the fake model provider this Trelis calls: the embedding
	// role points at it, so tests can fail embedding requests on purpose.
	provider *modeltest.FakeProvider
	// env is the process environment of every command run as this Trelis.
	env []string
	// bucket is this Trelis's own bucket on the shared silo server.
	bucket string
	// dbname is this Trelis's own database on the shared Postgres; the tests
	// also read it directly to see what ingest wrote.
	dbname string
	// taskQueue is this Trelis's Temporal task queue, named after dbname so
	// processes running side by side never take each other's tasks.
	taskQueue string
}

// listeningAddr returns the addr field of the JSON record Trelis logs once it
// accepts requests ("trelis listening", cmd/server/main.go), or "" for any
// other line. The port is only known from it, since Trelis listens on
// 127.0.0.1:0; the harness runs Trelis with TRELIS_LOG_FORMAT=json so this
// reads fields instead of matching console text.
func listeningAddr(line string) string {
	var rec struct {
		Msg  string `json:"msg"`
		Addr string `json:"addr"`
	}
	if json.Unmarshal([]byte(line), &rec) != nil || rec.Msg != "trelis listening" {
		return ""
	}
	return rec.Addr
}

// start boots Trelis on a fresh, empty database and stops it when the test
// ends, with its own bucket that Trelis creates on start. Neo4j and the
// silo server are shared by every test in the package.
func start(t *testing.T) *trelis {
	t.Helper()
	return startWith(t)
}

// startWith is start with extra TRELIS_* env, later entries overriding the
// harness's own.
func startWith(t *testing.T, extraEnv ...string) *trelis {
	t.Helper()
	tr := &trelis{provider: modeltest.NewFakeProvider(t)}
	dbname := freshDatabase(t)
	tr.bucket = strings.ReplaceAll(dbname, "_", "-")
	tr.dbname = dbname
	tr.taskQueue = dbname
	tr.env = append(trelisEnv(dbname, tr.bucket, tr.provider.URL()), extraEnv...)

	cmd := exec.Command(deps.binary, "-config", deps.config)
	cmd.Env = tr.env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	if err := cmd.Start(); err != nil {
		t.Fatalf("start trelis: %v", err)
	}
	exited := make(chan error, 1)
	addr := make(chan string, 1)
	var readers sync.WaitGroup
	readers.Add(2)
	// Logs go to stdout; stderr carries only startup failures (config and
	// logger errors), which the same buffer keeps for the failure message.
	go func() {
		defer readers.Done()
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			_, _ = fmt.Fprintln(&logs, line)
			if a := listeningAddr(line); a != "" {
				select {
				case addr <- a:
				default: // only the first one is read
				}
			}
		}
	}()
	go func() {
		defer readers.Done()
		_, _ = io.Copy(&logs, stderr)
	}()
	go func() {
		// Wait reads the pipes to EOF first; calling it before that is unsafe.
		readers.Wait()
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
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s:%s/trelis?sslmode=disable", pgUser, pgPassword, deps.pg.host, deps.pg.port))
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

// trelisEnv configures Trelis for the containers, dbname and bucket, as an
// operator would, through TRELIS_* env. TRELIS_* variables of the developer's
// shell are dropped, so every key Trelis reads is either set here or its
// default. Every test shares the package's Temporal dev server; the task
// queues are named after the test's database, so concurrent Trelis processes
// never take each other's workflow or model tasks. embeddingURL is the fake
// provider the embedding role calls.
func trelisEnv(dbname, bucket, embeddingURL string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TRELIS_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"TRELIS_SERVER_ADDR=127.0.0.1:0",
		"TRELIS_SERVER_SHUTDOWN_TIMEOUT=5s",
		// The harness reads stdout; a file would land in the test's working
		// directory and outlive it.
		"TRELIS_LOG_FILE_ENABLED=false",
		// listeningAddr parses the JSON record, not console text.
		"TRELIS_LOG_FORMAT=json",
		"TRELIS_POSTGRES_HOST="+deps.pg.host,
		"TRELIS_POSTGRES_PORT="+deps.pg.port,
		"TRELIS_POSTGRES_DBNAME="+dbname,
		"TRELIS_POSTGRES_USER="+pgUser,
		"TRELIS_POSTGRES_PASSWORD="+pgPassword,
		"TRELIS_POSTGRES_SSLMODE=disable",
		"TRELIS_NEO4J_HOST="+deps.neo4j.host,
		"TRELIS_NEO4J_PORT="+deps.neo4j.port,
		"TRELIS_NEO4J_USER=neo4j",
		"TRELIS_NEO4J_PASSWORD="+neo4jPassword,
		"TRELIS_TEMPORAL_HOST="+deps.temporal.host,
		"TRELIS_TEMPORAL_PORT="+deps.temporal.port,
		"TRELIS_TEMPORAL_TASK_QUEUE="+dbname,
		"TRELIS_TEMPORAL_MODEL_TASK_QUEUE="+dbname+"-models",
		"TRELIS_MODELS_EMBEDDING_BASE_URL="+embeddingURL,
		"TRELIS_MODELS_EMBEDDING_MODEL="+embeddingModel,
		"TRELIS_STORAGE_S3_ENDPOINT="+deps.s3,
		"TRELIS_STORAGE_S3_BUCKET="+bucket,
		"TRELIS_STORAGE_S3_ACCESS_KEY_ID="+s3User,
		"TRELIS_STORAGE_S3_SECRET_ACCESS_KEY="+s3Password,
		"TRELIS_STORAGE_S3_CREATE_BUCKET=true",
	)
}

// chunkContents returns this Trelis's stored Chunks for a Document in seq
// order, read straight from Postgres. The API does not serve Chunks yet, so
// this is how the ingest tests check what the workflow wrote.
func (tr *trelis) chunkContents(t *testing.T, documentID string) []string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, deps.pg.host, deps.pg.port, tr.dbname))
	if err != nil {
		t.Fatalf("connect to database %s: %v", tr.dbname, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, "SELECT content FROM chunks WHERE document_id = $1 ORDER BY seq", documentID)
	if err != nil {
		t.Fatalf("select chunks of %s: %v", documentID, err)
	}
	defer rows.Close()
	var contents []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatal(err)
		}
		contents = append(contents, content)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return contents
}

// admin runs `trelis admin <args>` against this Trelis's database, as an
// operator would, and returns its trimmed stdout. Admin logs go to stderr, so
// stdout carries only the command's own output.
func (tr *trelis) admin(t *testing.T, args ...string) string {
	t.Helper()
	stdout, err := run(deps.binary, tr.env, append([]string{"-config", deps.config, "admin"}, args...)...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return strings.TrimSpace(string(stdout))
}

// run executes the binary and returns its stdout. Errors include stderr +
// exit code so failures are debuggable without re-running.
//
// From Omnisc/WeKnora cli/acceptance/e2e/e2e_test.go,
// Copyright (C) 2026 Tencent, MIT license; see NOTICE.
func run(bin string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %v\nstderr:\n%s", filepath.Base(bin), strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
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

// post sends POST path with body as JSON and key as the bearer token (none
// when empty). The body is closed when the test ends.
func (tr *trelis) post(t *testing.T, path, key string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, tr.url+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
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
