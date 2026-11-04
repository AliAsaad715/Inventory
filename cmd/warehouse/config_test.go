package main

import (
	"log/slog"
	"strings"
	"testing"
)

// env is a getenv over a fixed set of variables.
func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		envDatabaseURL: "postgres://inventory:secret@inventory-db:5432/inventory",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.addr != ":50051" {
		t.Errorf("addr = %q, want :50051", cfg.addr)
	}
	if cfg.logLevel != slog.LevelInfo {
		t.Errorf("log level = %v, want INFO", cfg.logLevel)
	}
	if db := cfg.db.ConnConfig; db.User != "inventory" || db.Host != "inventory-db" || db.Port != 5432 || db.Database != "inventory" {
		t.Errorf("database = %s@%s:%d/%s, want inventory@inventory-db:5432/inventory", db.User, db.Host, db.Port, db.Database)
	}
}

func TestLoadConfigReadsEveryVariable(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		envAddr:        "127.0.0.1:6000",
		envLogLevel:    "debug",
		envDatabaseURL: "host=db port=5433 dbname=stock user=warehouse password=pw pool_max_conns=8",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.addr != "127.0.0.1:6000" {
		t.Errorf("addr = %q, want 127.0.0.1:6000", cfg.addr)
	}
	if cfg.logLevel != slog.LevelDebug {
		t.Errorf("log level = %v, want DEBUG", cfg.logLevel)
	}
	if cfg.db.ConnConfig.Port != 5433 || cfg.db.ConnConfig.Database != "stock" {
		t.Errorf("database = %s:%d, want stock on 5433", cfg.db.ConnConfig.Database, cfg.db.ConnConfig.Port)
	}
	if cfg.db.MaxConns != 8 {
		t.Errorf("pool size = %d, want the 8 from pool_max_conns", cfg.db.MaxConns)
	}
}

func TestLoadConfigReportsEveryProblemAtOnce(t *testing.T) {
	_, err := loadConfig(env(map[string]string{
		envAddr:     "50051",
		envLogLevel: "verbose",
	}))
	if err == nil {
		t.Fatal("loadConfig accepted a broken configuration")
	}

	for _, want := range []string{envAddr, envLogLevel, envDatabaseURL + " is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error doesn't mention %s:\n%v", want, err)
		}
	}
}

func TestLoadConfigKeepsThePasswordOutOfErrors(t *testing.T) {
	_, err := loadConfig(env(map[string]string{
		envDatabaseURL: "postgres://inventory:s3cret-pw@inventory-db:not-a-port/inventory",
	}))
	if err == nil {
		t.Fatal("loadConfig accepted a URL with an invalid port")
	}

	if strings.Contains(err.Error(), "s3cret-pw") {
		t.Errorf("the error shows the password: %v", err)
	}
}
