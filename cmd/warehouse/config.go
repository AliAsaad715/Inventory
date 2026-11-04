package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The warehouse reads its settings from these environment variables. Compose sets them,
// and the database password comes from .env, as it does for the app.
const (
	envAddr        = "INVENTORY_GRPC_ADDR"    // optional: where the gRPC server listens
	envDatabaseURL = "INVENTORY_DATABASE_URL" // required: the warehouse's own database
	envLogLevel    = "INVENTORY_LOG_LEVEL"    // optional: debug, info, warn or error
)

const defaultAddr = ":50051"

type config struct {
	addr     string
	logLevel slog.Level
	db       *pgxpool.Config
}

// loadConfig reads the settings through getenv (os.Getenv outside tests) and reports
// every problem at once, so a broken setup is fixed in one pass.
func loadConfig(getenv func(string) string) (config, error) {
	var cfg config
	var errs []error

	cfg.addr = listenAddr(getenv)
	if _, _, err := net.SplitHostPort(cfg.addr); err != nil {
		errs = append(errs, fmt.Errorf("%s %q: %w", envAddr, cfg.addr, err))
	}

	if level := getenv(envLogLevel); level != "" {
		if err := cfg.logLevel.UnmarshalText([]byte(level)); err != nil {
			errs = append(errs, fmt.Errorf("%s must be debug, info, warn or error, not %q", envLogLevel, level))
		}
	}

	// A postgres:// URL or a key=value string; pgx reads pool settings such as pool_max_conns from it too
	if dsn := strings.TrimSpace(getenv(envDatabaseURL)); dsn == "" {
		errs = append(errs, fmt.Errorf("%s is required", envDatabaseURL))
	} else if db, err := pgxpool.ParseConfig(dsn); err != nil {
		// pgx masks the password in its parse errors
		errs = append(errs, fmt.Errorf("%s: %w", envDatabaseURL, err))
	} else {
		cfg.db = db
	}

	return cfg, errors.Join(errs...)
}

// listenAddr is the configured gRPC address or the default; the healthcheck reads it too.
func listenAddr(getenv func(string) string) string {
	if addr := getenv(envAddr); addr != "" {
		return addr
	}
	return defaultAddr
}
