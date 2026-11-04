package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	inventoryv1 "github.com/AliAsaad715/Shop/warehouse/gen/inventory/v1"
)

// newServer builds the gRPC server: the inventory service, the standard health service,
// and the interceptors that log each call and contain a panicking one. Health starts as
// NOT_SERVING; watchDatabase turns it SERVING once the database answers.
func newServer(log *slog.Logger, inv inventoryv1.InventoryServiceServer) (*grpc.Server, *health.Server) {
	srv := grpc.NewServer(
		// The first runs outermost, so the log line also records a call that panicked
		grpc.ChainUnaryInterceptor(logCalls(log), recoverPanics(log)),
	)
	inventoryv1.RegisterInventoryServiceServer(srv, inv)

	hs := health.NewServer()
	setHealth(hs, healthpb.HealthCheckResponse_NOT_SERVING)
	healthpb.RegisterHealthServer(srv, hs)
	return srv, hs
}

// setHealth sets the status of the server as a whole (the empty name, which the
// healthcheck asks for) and of the inventory service by name.
func setHealth(hs *health.Server, status healthpb.HealthCheckResponse_ServingStatus) {
	hs.SetServingStatus("", status)
	hs.SetServingStatus(inventoryv1.InventoryService_ServiceDesc.ServiceName, status)
}

// serve runs srv on lis until ctx ends, then stops gracefully: health turns NOT_SERVING,
// new calls are refused, and calls in flight get up to grace to finish. After that their
// contexts are cancelled, which cancels their queries too. It returns nil after a stop.
func serve(ctx context.Context, srv *grpc.Server, hs *health.Server, lis net.Listener, grace time.Duration, log *slog.Logger) error {
	served := make(chan error, 1)
	go func() { served <- srv.Serve(lis) }()

	select {
	case err := <-served:
		srv.Stop()
		return fmt.Errorf("serve gRPC: %w", err)
	case <-ctx.Done():
	}

	start := time.Now()
	log.Info("shutting down", "grace", grace.String())
	hs.Shutdown() // NOT_SERVING from here on, whatever the database watcher reports

	drained := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(grace):
		log.Warn("calls still running after the grace period; cancelling them", "grace", grace.String())
		srv.Stop()
		<-drained // returns once the cancelled handlers have
	}
	log.Info("stopped", "took_ms", milliseconds(time.Since(start)))
	return nil
}
