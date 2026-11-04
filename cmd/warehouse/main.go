// Command warehouse serves inventory.v1.InventoryService over gRPC from its own PostgreSQL
// database. With no arguments it runs the server; `warehouse healthcheck` is the image's
// HEALTHCHECK probe, since the distroless image has no shell or curl to run one.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AliAsaad715/Shop/warehouse/internal/inventory"
)

// shutdownGrace bounds the drain after SIGTERM. Calls carry the app's deadlines (2 s at
// most) and Docker kills the container 10 s after SIGTERM, so 5 s fits between the two.
const shutdownGrace = 5 * time.Second

func main() {
	switch args := os.Args[1:]; {
	case len(args) == 1 && args[0] == "healthcheck":
		os.Exit(healthcheck(os.Getenv, os.Stdout, os.Stderr))
	case len(args) > 0:
		fmt.Fprintln(os.Stderr, "usage: warehouse [healthcheck]")
		os.Exit(2)
	}

	cfg, err := loadConfig(os.Getenv)
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.logLevel}))
	slog.SetDefault(log)
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	// Docker stops a container with SIGTERM; Ctrl+C sends SIGINT
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first signal, a second one ends the process at once instead of waiting for the drain
	context.AfterFunc(ctx, stop)

	if err := run(ctx, cfg, log); err != nil {
		log.Error("warehouse failed", "error", err)
		os.Exit(1)
	}
}

// run serves until ctx ends, then stops gracefully. Startup doesn't wait for the database:
// health reports NOT_SERVING until it answers, then turns SERVING on its own.
func run(ctx context.Context, cfg config, log *slog.Logger) error {
	// Connects lazily, so this fails only on a bad configuration, never on a database that is down
	pool, err := pgxpool.NewWithConfig(ctx, cfg.db)
	if err != nil {
		return fmt.Errorf("create database pool: %w", err)
	}
	defer pool.Close()

	lis, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.addr, err)
	}

	srv, hs := newServer(log, inventory.NewServer())

	// Stopped and waited for before the deferred pool.Close, so no ping outlives the pool
	watchCtx, stopWatching := context.WithCancel(ctx)
	var watcher sync.WaitGroup
	watcher.Go(func() { watchDatabase(watchCtx, pool, hs, healthInterval, log) })
	defer watcher.Wait()
	defer stopWatching()

	log.Info("warehouse started",
		"addr", lis.Addr().String(),
		"db_host", cfg.db.ConnConfig.Host,
		"db_port", cfg.db.ConnConfig.Port,
		"db_name", cfg.db.ConnConfig.Database)
	return serve(ctx, srv, hs, lis, shutdownGrace, log)
}
