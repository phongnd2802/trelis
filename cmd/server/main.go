// Command server builds the trelis binary (bin/trelis from make build,
// /trelis in the image; `go run ./cmd/server` in development), which runs the
// Trelis HTTP service.
//
//	trelis -config config/trelis.yaml
//	trelis -config config/trelis.yaml admin <command>
//
// The admin commands create Principals, Groups, memberships and API keys
// (run `trelis admin` for the list); they migrate Postgres and exit without
// serving. Every HTTP route except /healthz and /readyz needs an API key.
//
// On start it applies pending Postgres migrations, then serves. The ingest
// workflow's worker runs in the process, retrying in the background while
// Temporal is unreachable, as the bucket is created while object storage is.
// GET /readyz probes Postgres, object storage, Neo4j and Temporal; the last
// three are reached lazily, so the service starts even while they are still
// coming up.
// SIGINT or SIGTERM stops accepting connections, drains in-flight requests
// for up to server.shutdown_timeout and stops the worker before exiting.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/application/service"
	"github.com/phongnd2802/trelis/internal/application/service/file"
	"github.com/phongnd2802/trelis/internal/config"
	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/handler"
	"github.com/phongnd2802/trelis/internal/ingest"
	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/models/chat"
	"github.com/phongnd2802/trelis/internal/models/embedding"
	"github.com/phongnd2802/trelis/internal/models/rerank"
	"github.com/phongnd2802/trelis/internal/parser"
	"github.com/phongnd2802/trelis/internal/router"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// bucketRetry is how long ensureBucket waits between attempts.
const bucketRetry = 2 * time.Second

// ensureBucket creates the bucket if it is missing. The first attempt runs
// before serving, so a reachable store has its bucket before any upload;
// if it fails, Trelis serves anyway (storage shows down in /readyz, as a
// Neo4j or Temporal still coming up does) and keeps trying until ctx ends.
func ensureBucket(ctx context.Context, files *file.S3FileService) {
	err := files.EnsureBucket(ctx)
	if err == nil {
		return
	}
	logger.Ctx(ctx).Warn("storage: bucket not ready, retrying in the background", zap.Error(err))
	go func() {
		t := time.NewTicker(bucketRetry)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if err := files.EnsureBucket(ctx); err != nil {
				logger.Ctx(ctx).Debug("storage: bucket still not ready", zap.Error(err))
				continue
			}
			logger.Ctx(ctx).Info("storage: bucket ready")
			return
		}
	}()
}

// workerRetry is how long startWorker waits between attempts while Temporal
// is unreachable.
const workerRetry = 2 * time.Second

// startWorker runs one Temporal worker in the background, retrying while
// Temporal is unreachable, the way ensureBucket waits for object storage: the
// service starts and serves either way, and /readyz reports Temporal. name
// says which worker a record is about. The returned stop function cancels the
// retries, stops a running worker and waits for it, so shutdown finishes
// before its Temporal client closes.
func startWorker(ctx context.Context, name, taskQueue string, newWorker func() worker.Worker) (stop func()) {
	workerCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		first := true
		for {
			w := newWorker()
			err := w.Start()
			if err == nil {
				logger.Ctx(ctx).Info("temporal worker started",
					zap.String("worker", name), zap.String("task_queue", taskQueue))
				<-workerCtx.Done()
				w.Stop()
				return
			}
			// One warning when Temporal is first found down, then debug: a
			// long outage should not fill the log with the same line.
			if first {
				logger.Ctx(ctx).Warn("temporal worker not ready, retrying in the background",
					zap.String("worker", name), zap.Error(err))
				first = false
			} else {
				logger.Ctx(ctx).Debug("temporal worker still not ready",
					zap.String("worker", name), zap.Error(err))
			}
			select {
			case <-workerCtx.Done():
				return
			case <-time.After(workerRetry):
			}
		}
	}()
	return func() {
		cancel()
		<-stopped
	}
}

// startReconciler runs one Reconcile pass in the background, then one every
// interval, until ctx ends. Every replica runs it; every step is idempotent,
// so two replicas cannot start a workflow twice or delete a live prefix
// (ADR-0005, ADR-0006). A failed pass logs at warn and the next tick tries
// again. The returned stop function cancels the loop and waits for a pass in
// flight, so shutdown finishes before its Temporal client closes.
func startReconciler(ctx context.Context, svc interfaces.ReconcileService, interval time.Duration) (stop func()) {
	loopCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			// A pass cut short by shutdown is not a failure.
			if err := svc.Reconcile(loopCtx); err != nil && loopCtx.Err() == nil {
				logger.Ctx(ctx).Warn("reconciler pass failed", zap.Error(err))
			}
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-stopped
	}
}

// readyTimeout bounds each dependency probe behind /readyz.
const readyTimeout = 2 * time.Second

func main() {
	configPath := flag.String("config", "config/trelis.yaml", "path to the YAML config file")
	flag.Parse()

	if err := run(*configPath, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "trelis:", err)
		os.Exit(1)
	}
}

// logOutput is where the logger's stdout core writes. An admin command logs to
// stderr so the command's result on stdout stays machine-readable; the server
// logs to stdout.
func logOutput(admin bool) io.Writer {
	if admin {
		return os.Stderr
	}
	return os.Stdout
}

// logExit records a failed server run once, before main prints it to stderr.
// Admin errors are operator mistakes that main already prints, so they are not
// logged here. The stacktrace is suppressed because at this point it would
// only show this defer, not the failure.
func logExit(ctx context.Context, admin bool, err error) {
	if admin || err == nil {
		return
	}
	logger.Ctx(ctx).
		WithOptions(zap.AddStacktrace(zapcore.FatalLevel)).
		Error("trelis exited with an error", zap.Error(err))
}

func run(configPath string, args []string) (err error) {
	if len(args) > 0 && args[0] != "admin" {
		return fmt.Errorf("unknown command %q (want none, or admin)", args[0])
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	l, closeLog, err := logger.NewWithWriter(cfg.Log, logOutput(len(args) > 0))
	if err != nil {
		return err
	}
	zap.ReplaceGlobals(l)
	defer func() { _ = closeLog() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Log the returned error once it can reach the file as well as main's
	// stderr. Registered after closeLog so it runs before the file closes.
	defer func() { logExit(ctx, len(args) > 0, err) }()
	// After the first signal, restore default handling so a second one
	// force-quits instead of waiting out the drain.
	go func() { <-ctx.Done(); stop() }()

	pool, err := db.Open(ctx, cfg.Postgres.DSN())
	if err != nil {
		return err
	}
	defer pool.Close()
	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	// CI's compose smoke test greps this record for "applied":0.
	logger.Ctx(ctx).Info("postgres migrated", zap.Int("applied", applied))
	dbStore := db.NewStore(pool)
	principalRepo, keyRepo := repository.NewPrincipalRepository(dbStore), repository.NewAPIKeyRepository(dbStore)
	principals := service.NewPrincipalService(dbStore, principalRepo, keyRepo)

	if len(args) > 0 {
		return admin(ctx, principals, args[1:], os.Stdout)
	}

	graph, err := neo4j.NewDriver(cfg.Neo4j.URI(), neo4j.BasicAuth(cfg.Neo4j.User, cfg.Neo4j.Password, ""))
	if err != nil {
		return fmt.Errorf("neo4j: %w", err)
	}
	defer func() { _ = graph.Close(context.Background()) }()

	temporal, err := client.NewLazyClient(client.Options{
		HostPort:  cfg.Temporal.HostPort(),
		Namespace: cfg.Temporal.Namespace,
		Logger:    logger.NewTemporal(l),
	})
	if err != nil {
		return fmt.Errorf("temporal: %w", err)
	}
	defer temporal.Close()

	s3 := cfg.Storage.S3
	files, err := file.NewS3FileService(file.S3Options{
		Endpoint:        s3.Endpoint,
		Region:          s3.Region,
		Bucket:          s3.Bucket,
		AccessKeyID:     s3.AccessKeyID,
		SecretAccessKey: s3.SecretAccessKey,
		ForcePathStyle:  s3.ForcePathStyle,
	})
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if s3.CreateBucket {
		ensureBucket(ctx, files)
	}
	docRepo := repository.NewDocumentRepository(dbStore)
	// The embedder is optional: without a configured embedding role, ingest
	// still parses and chunks, and the embed step fails the Document with a
	// reason an operator can act on.
	var embedder embedding.Embedder
	if cfg.Models.Embedding.Configured() {
		embedder, err = embedding.New(cfg.Models.Embedding)
		if err != nil {
			return fmt.Errorf("embedding model: %w", err)
		}
	} else {
		logger.Ctx(ctx).Warn("no embedding model configured; Documents will fail ingest until one is set")
	}
	// The reranker is optional too: without a configured rerank role, a
	// request that names rerank answers service unavailable instead of
	// silently skipping the stage.
	var reranker rerank.Reranker
	if cfg.Models.Rerank.Configured() {
		reranker, err = rerank.New(cfg.Models.Rerank)
		if err != nil {
			return fmt.Errorf("rerank model: %w", err)
		}
	}
	// The chat role is optional as well: without it /answer answers service
	// unavailable instead of inventing an answer.
	var answerGenerator interfaces.Generator
	if cfg.Models.Chat.Configured() {
		chatClient, err := chat.NewChat("models.chat", cfg.Models.Chat)
		if err != nil {
			return fmt.Errorf("chat model: %w", err)
		}
		answerGenerator = chat.Generator(chatClient)
	} else {
		logger.Ctx(ctx).Warn("no chat model configured; pipeline answers will answer service unavailable")
	}
	starter := ingest.NewStarter(temporal, cfg.Temporal.TaskQueue)
	// One Starter serves both roles: the ingest start and the delete cleanup.
	documentService := service.NewDocumentService(dbStore, docRepo, principalRepo, files, starter, starter)
	ingestActivities := ingest.NewActivities(service.NewIngestService(dbStore, docRepo,
		repository.NewChunkRepository(dbStore), files, parser.NewFormatRouter(false), embedder,
		cfg.Chunking.SplitterConfig()), documentService)
	stopIngestWorker := startWorker(ctx, "ingest", cfg.Temporal.TaskQueue, func() worker.Worker {
		return ingest.NewWorker(temporal, cfg.Temporal.TaskQueue, cfg.Temporal.ModelTaskQueue,
			cfg.Models.Embedding.BatchSize, ingestActivities)
	})
	defer stopIngestWorker()
	stopModelWorker := startWorker(ctx, "models", cfg.Temporal.ModelTaskQueue, func() worker.Worker {
		return ingest.NewModelWorker(temporal, cfg.Temporal.ModelTaskQueue,
			cfg.Temporal.ModelActivitiesPerSecond, ingestActivities)
	})
	defer stopModelWorker()
	stopReconciler := startReconciler(ctx,
		service.NewReconcileService(docRepo, files, starter), cfg.Reconciler.Interval)
	defer stopReconciler()
	docs := handler.NewDocumentHandler(documentService, cfg.Storage.MaxFileSize())
	retrievalService := service.NewRetrievalService(docRepo, repository.NewChunkSearcher(dbStore), embedder, reranker)
	retrieval := handler.NewRetrievalHandler(retrievalService)
	answers := handler.NewAnswerHandler(service.NewAnswerService(retrievalService, answerGenerator))
	chunks := handler.NewChunkHandler(service.NewChunkService(repository.NewChunkReader(dbStore)))

	ready := router.Readiness{Timeout: readyTimeout, Checks: []router.Check{
		{Name: "postgres", Probe: pool.Ping},
		{Name: "storage", Probe: files.CheckConnectivity},
		{Name: "neo4j", Probe: graph.VerifyConnectivity},
		{Name: "temporal", Probe: func(ctx context.Context) error {
			_, err := temporal.CheckHealth(ctx, &client.CheckHealthRequest{})
			return err
		}},
	}}

	ln, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Server.Addr, err)
	}
	// The integration harness reads addr from this record (listeningAddr);
	// keep the message and the field name.
	logger.Ctx(ctx).Info("trelis listening", zap.String("addr", ln.Addr().String()), zap.String("config", configPath))

	if err := router.Serve(ctx, ln, router.New(ready, service.NewAuthService(principalRepo, keyRepo),
		handler.NewPrincipalHandler(principals), docs, retrieval, answers, chunks,
		cfg.Server.Swagger), cfg.Server.ShutdownTimeout); err != nil {
		return err
	}
	logger.Ctx(ctx).Info("trelis stopped")
	return nil
}
