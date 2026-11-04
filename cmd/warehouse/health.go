package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	// healthInterval is how often the watcher pings the database.
	healthInterval = 5 * time.Second
	// pingTimeout bounds one ping, so a database that hangs counts as down.
	pingTimeout = 2 * time.Second
	// probeTimeout keeps the healthcheck inside the 3 s HEALTHCHECK --timeout in the Dockerfile.
	probeTimeout = 2 * time.Second
)

// pinger is the part of *pgxpool.Pool the watcher needs; tests pass a fake.
type pinger interface {
	Ping(ctx context.Context) error
}

// watchDatabase keeps the health status in step with the database until ctx ends:
// SERVING while pings succeed, NOT_SERVING while they fail. No call can succeed without
// the database, so the warehouse reports an outage as unhealthy and recovers on its own.
// It logs the changes, not every ping.
func watchDatabase(ctx context.Context, db pinger, hs *health.Server, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var current healthpb.HealthCheckResponse_ServingStatus // UNKNOWN until the first ping
	for {
		pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
		err := db.Ping(pingCtx)
		cancel()
		if ctx.Err() != nil {
			return // shutting down: the ping was cancelled, not refused
		}

		next := healthpb.HealthCheckResponse_SERVING
		if err != nil {
			next = healthpb.HealthCheckResponse_NOT_SERVING
		}
		if next != current {
			if err != nil {
				log.Warn("database unreachable; health is NOT_SERVING", "error", err)
			} else {
				log.Info("database reachable; health is SERVING")
			}
			setHealth(hs, next)
			current = next
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// healthcheck asks the server in this container for its health, prints the answer, and
// returns the exit code Docker's HEALTHCHECK reads: 0 when SERVING, 1 otherwise.
func healthcheck(getenv func(string) string, stdout, stderr io.Writer) int {
	target, err := probeTarget(listenAddr(getenv))
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck:", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	status, err := checkHealth(ctx, target)
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck:", err)
		return 1
	}
	if status != healthpb.HealthCheckResponse_SERVING {
		fmt.Fprintln(stderr, "healthcheck:", status)
		return 1
	}
	fmt.Fprintln(stdout, status)
	return 0
}

// checkHealth calls grpc.health.v1.Health/Check for the server as a whole.
func checkHealth(ctx context.Context, target string) (healthpb.HealthCheckResponse_ServingStatus, error) {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return healthpb.HealthCheckResponse_UNKNOWN, err
	}
	defer conn.Close()

	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return healthpb.HealthCheckResponse_UNKNOWN, err
	}
	return resp.GetStatus(), nil
}

// probeTarget turns the listen address into one the probe can dial from inside the
// container: a wildcard host (":50051", "0.0.0.0:50051", "[::]:50051") becomes loopback.
func probeTarget(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", envAddr, listenAddr, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}
