// Command server runs the Trelis HTTP service.
//
//	server -config config/trelis.yaml
//	server -config config/trelis.yaml admin <command>
//
// The admin commands create Principals, Groups, memberships and API keys
// (run `server admin` for the list); they migrate Postgres and exit without
// serving. Every HTTP route except /healthz and /readyz needs an API key.
//
// On start it applies pending Postgres migrations, then serves. GET /readyz
// probes Postgres, Neo4j and Temporal; Neo4j and Temporal are connected
// lazily, so the service starts even while they are still coming up.
// SIGINT or SIGTERM stops accepting connections and drains in-flight
// requests for up to server.shutdown_timeout before exiting.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
	"go.temporal.io/sdk/client"
	temporallog "go.temporal.io/sdk/log"

	"github.com/phongnd2802/trelis/internal/auth"
	"github.com/phongnd2802/trelis/internal/config"
	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/documents"
	"github.com/phongnd2802/trelis/internal/router"
)

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

func run(configPath string, args []string) error {
	if len(args) > 0 && args[0] != "admin" {
		return fmt.Errorf("unknown command %q (want none, or admin)", args[0])
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	slog.Info("postgres migrated", "applied", applied)
	store := auth.NewStore(pool)

	if len(args) > 0 {
		return admin(ctx, store, args[1:], os.Stdout)
	}

	graph, err := neo4j.NewDriver(cfg.Neo4j.URI(), neo4j.BasicAuth(cfg.Neo4j.User, cfg.Neo4j.Password, ""))
	if err != nil {
		return fmt.Errorf("neo4j: %w", err)
	}
	defer func() { _ = graph.Close(context.Background()) }()

	temporal, err := client.NewLazyClient(client.Options{
		HostPort:  cfg.Temporal.HostPort(),
		Namespace: cfg.Temporal.Namespace,
		Logger:    temporallog.NewStructuredLogger(slog.Default()),
	})
	if err != nil {
		return fmt.Errorf("temporal: %w", err)
	}
	defer temporal.Close()

	ready := router.Readiness{Timeout: readyTimeout, Checks: []router.Check{
		{Name: "postgres", Probe: pool.Ping},
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
	slog.Info("trelis listening", "addr", ln.Addr().String(), "config", configPath)

	if err := router.Serve(ctx, ln, router.New(ready, store, documents.NewStore(pool)), cfg.Server.ShutdownTimeout); err != nil {
		return err
	}
	slog.Info("trelis stopped")
	return nil
}
