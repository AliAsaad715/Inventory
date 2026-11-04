package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	inventoryv1 "github.com/AliAsaad715/Shop/warehouse/gen/inventory/v1"
	"github.com/AliAsaad715/Shop/warehouse/internal/inventory"
)

func TestSkeletonAnswersUnimplementedAndLogsTheOrder(t *testing.T) {
	addr, _, logs := startServer(t, inventory.NewServer())
	client := inventoryv1.NewInventoryServiceClient(dial(t, addr))

	const orderID = "3f8e2a64-9b1c-4c2e-8f7d-5a6b7c8d9e0f"
	_, err := client.Reserve(t.Context(), &inventoryv1.ReserveRequest{OrderId: orderID})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("Reserve = %v, want UNIMPLEMENTED until R2-04", err)
	}

	calls := logs.entries(t, "grpc call")
	if len(calls) != 1 {
		t.Fatalf("logged %d calls, want 1", len(calls))
	}
	for key, want := range map[string]any{
		"level":    "ERROR",
		"method":   inventoryv1.InventoryService_Reserve_FullMethodName,
		"code":     "Unimplemented",
		"order_id": orderID,
	} {
		if got := calls[0][key]; got != want {
			t.Errorf("log %s = %v, want %v", key, got, want)
		}
	}
}

func TestPanicFailsOnlyThatCall(t *testing.T) {
	addr, _, logs := startServer(t, panickingInventory{})
	conn := dial(t, addr)

	_, err := inventoryv1.NewInventoryServiceClient(conn).GetStock(t.Context(), &inventoryv1.GetStockRequest{ProductId: "p-1"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("GetStock = %v, want INTERNAL", err)
	}
	if _, err := healthpb.NewHealthClient(conn).Check(t.Context(), &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("the server stopped answering after the panic: %v", err)
	}

	panics := logs.entries(t, "handler panicked")
	if len(panics) != 1 {
		t.Fatalf("logged %d panics, want 1", len(panics))
	}
	if stack, _ := panics[0]["stack"].(string); !strings.Contains(stack, "GetStock") {
		t.Errorf("the panic line has no stack through GetStock: %v", panics[0])
	}
	if calls := logs.entries(t, "grpc call"); len(calls) == 0 || calls[0]["code"] != "Internal" || calls[0]["product_id"] != "p-1" {
		t.Errorf("want the call logged as Internal with its product_id, got %v", calls)
	}
}

func TestServeLetsCallsInFlightFinish(t *testing.T) {
	inv := newSlowInventory(300 * time.Millisecond)
	ctx, stop := context.WithCancel(t.Context())
	addr, hs, served := startServing(t, ctx, inv, 5*time.Second)
	setHealth(hs, healthpb.HealthCheckResponse_SERVING)

	reply := reserveAsync(t, addr)
	<-inv.started
	stop() // what SIGTERM does in main

	if err := <-reply; err != nil {
		t.Errorf("the call in flight failed during the drain: %v", err)
	}
	if err := <-served; err != nil {
		t.Errorf("serve = %v, want nil after a clean stop", err)
	}
	wantHealth(t, hs, healthpb.HealthCheckResponse_NOT_SERVING)
}

func TestServeCancelsCallsThatOutlastTheGrace(t *testing.T) {
	const grace = 200 * time.Millisecond
	inv := newSlowInventory(time.Minute)
	ctx, stop := context.WithCancel(t.Context())
	addr, _, served := startServing(t, ctx, inv, grace)

	reply := reserveAsync(t, addr)
	<-inv.started
	start := time.Now()
	stop()

	select {
	case <-inv.cancelled: // a query running on this context would be cancelled with it
	case <-time.After(5 * time.Second):
		t.Fatal("the call outlived the grace period")
	}
	if err := <-reply; err == nil {
		t.Error("the call that was cut off succeeded")
	}
	if err := <-served; err != nil {
		t.Errorf("serve = %v, want nil after a forced stop", err)
	}
	if took := time.Since(start); took < grace || took > grace+2*time.Second {
		t.Errorf("the stop took %v, want about the %v grace", took, grace)
	}
}

func TestServeReturnsTheServeError(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lis.Close() // Serve fails at once on a closed listener

	srv, hs := newServer(discardLogger(), inventory.NewServer())
	if err := serve(t.Context(), srv, hs, lis, time.Second, discardLogger()); err == nil {
		t.Error("serve = nil, want the error from Serve")
	}
}

// panickingInventory's GetStock panics, standing in for a bug in a handler.
type panickingInventory struct {
	inventoryv1.UnimplementedInventoryServiceServer
}

func (panickingInventory) GetStock(context.Context, *inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
	panic("boom")
}

// slowInventory's Reserve takes delay to answer, or gives up when its context ends.
type slowInventory struct {
	inventoryv1.UnimplementedInventoryServiceServer
	delay     time.Duration
	started   chan struct{} // closed when Reserve begins
	cancelled chan struct{} // closed when Reserve saw its context end
}

func newSlowInventory(delay time.Duration) *slowInventory {
	return &slowInventory{delay: delay, started: make(chan struct{}), cancelled: make(chan struct{})}
}

func (s *slowInventory) Reserve(ctx context.Context, _ *inventoryv1.ReserveRequest) (*inventoryv1.ReserveResponse, error) {
	close(s.started)
	select {
	case <-time.After(s.delay):
		return &inventoryv1.ReserveResponse{Reserved: true}, nil
	case <-ctx.Done():
		close(s.cancelled)
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

// reserveAsync sends one Reserve and returns a channel with its error.
func reserveAsync(t *testing.T, addr string) <-chan error {
	t.Helper()
	client := inventoryv1.NewInventoryServiceClient(dial(t, addr))
	reply := make(chan error, 1)
	go func() {
		_, err := client.Reserve(context.Background(), &inventoryv1.ReserveRequest{OrderId: "order-1"})
		reply <- err
	}()
	return reply
}

// startServer serves newServer on a free loopback port until the test ends, and returns
// its address, its health server and its log.
func startServer(t *testing.T, inv inventoryv1.InventoryServiceServer) (string, *health.Server, *logBuffer) {
	t.Helper()
	logs, log := captureLogs()
	srv, hs := newServer(log, inv)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), hs, logs
}

// startServing runs serve, as main does, until ctx ends; the channel gets what it returns.
func startServing(t *testing.T, ctx context.Context, inv inventoryv1.InventoryServiceServer, grace time.Duration) (string, *health.Server, <-chan error) {
	t.Helper()
	srv, hs := newServer(discardLogger(), inv)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, hs, lis, grace, discardLogger()) }()
	return lis.Addr().String(), hs, served
}

func dial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// captureLogs returns a JSON logger at debug level and the buffer it writes to.
func captureLogs() (*logBuffer, *slog.Logger) {
	logs := &logBuffer{}
	return logs, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// logBuffer collects JSON log lines; the mutex lets a test read while the server writes.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// entries decodes the lines whose msg is msg.
func (b *logBuffer) entries(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()

	var found []map[string]any
	for line := range bytes.Lines(b.buf.Bytes()) {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("log line %q isn't JSON: %v", line, err)
		}
		if entry["msg"] == msg {
			found = append(found, entry)
		}
	}
	return found
}
