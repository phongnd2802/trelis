// Command server runs the Trelis HTTP service.
//
//	server -config config/trelis.yaml
//
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

	"github.com/phongnd2802/trelis/internal/config"
	"github.com/phongnd2802/trelis/internal/router"
)

func main() {
	configPath := flag.String("config", "config/trelis.yaml", "path to the YAML config file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "trelis:", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default handling so a second one
	// force-quits instead of waiting out the drain.
	go func() { <-ctx.Done(); stop() }()

	ln, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Server.Addr, err)
	}
	slog.Info("trelis listening", "addr", ln.Addr().String(), "config", configPath)

	if err := router.Serve(ctx, ln, router.New(), cfg.Server.ShutdownTimeout); err != nil {
		return err
	}
	slog.Info("trelis stopped")
	return nil
}
