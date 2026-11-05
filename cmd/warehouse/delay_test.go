package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	inventoryv1 "github.com/AliAsaad715/Shop/warehouse/gen/inventory/v1"
)

func TestDelayHoldsInventoryCallsButNotHealthChecks(t *testing.T) {
	const delay = 300 * time.Millisecond
	addr, hs, _ := startDelayedServer(t, &countingInventory{}, delay)
	setHealth(hs, healthpb.HealthCheckResponse_SERVING)
	conn := dial(t, addr)

	start := time.Now()
	if _, err := inventoryv1.NewInventoryServiceClient(conn).GetStock(t.Context(), &inventoryv1.GetStockRequest{ProductId: "p-1"}); err != nil {
		t.Fatalf("GetStock: %v", err)
	}
	if took := time.Since(start); took < delay {
		t.Errorf("GetStock answered after %v, want at least the %v delay", took, delay)
	}

	start = time.Now()
	if _, err := healthpb.NewHealthClient(conn).Check(t.Context(), &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("health check: %v", err)
	}
	if took := time.Since(start); took >= delay {
		t.Errorf("the health check took %v; it must not wait for the %v delay", took, delay)
	}
}

func TestDelayPastTheDeadlineNeverReachesTheHandler(t *testing.T) {
	inv := &countingInventory{}
	addr, _, logs := startDelayedServer(t, inv, time.Second)
	client := inventoryv1.NewInventoryServiceClient(dial(t, addr))

	const orderID = "7c1d9a52-3e4f-4b6a-9d8c-2f1e0a9b8c7d"
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := client.Reserve(ctx, &inventoryv1.ReserveRequest{OrderId: orderID})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("Reserve = %v, want DEADLINE_EXCEEDED", err)
	}

	// The server writes its log line once the call is over, handler or not
	var calls []map[string]any
	for deadline := time.Now().Add(2 * time.Second); len(calls) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		calls = logs.entries(t, "grpc call")
	}
	if len(calls) != 1 {
		t.Fatalf("logged %d calls, want 1", len(calls))
	}
	// The caller's cancel, sent at its deadline, usually arrives before the server's copy of that
	// deadline fires, so the line says Canceled as often as DeadlineExceeded
	if code := calls[0]["code"]; (code != "Canceled" && code != "DeadlineExceeded") || calls[0]["order_id"] != orderID {
		t.Errorf("want the call logged as Canceled or DeadlineExceeded with its order_id, got %v", calls[0])
	}
	if n := inv.reserves.Load(); n != 0 {
		t.Errorf("Reserve reached its handler %d times; a call that timed out while delayed must change no stock", n)
	}
}

// countingInventory answers at once and counts the Reserve calls that reach it.
type countingInventory struct {
	inventoryv1.UnimplementedInventoryServiceServer
	reserves atomic.Int32
}

func (*countingInventory) GetStock(context.Context, *inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
	return &inventoryv1.GetStockResponse{Available: 1}, nil
}

func (c *countingInventory) Reserve(context.Context, *inventoryv1.ReserveRequest) (*inventoryv1.ReserveResponse, error) {
	c.reserves.Add(1)
	return &inventoryv1.ReserveResponse{Reserved: true}, nil
}
