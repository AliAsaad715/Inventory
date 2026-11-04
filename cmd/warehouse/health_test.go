package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	inventoryv1 "github.com/AliAsaad715/Shop/warehouse/gen/inventory/v1"
	"github.com/AliAsaad715/Shop/warehouse/internal/inventory"
)

// fakeDB answers each ping with the error the test set last.
type fakeDB struct {
	mu  sync.Mutex
	err error
}

func (f *fakeDB) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeDB) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func TestWatchDatabaseFollowsTheDatabase(t *testing.T) {
	// synctest runs the watcher on a fake clock: each 5 s sleep passes at once, and
	// synctest.Wait returns when the watcher is blocked waiting for its next tick
	synctest.Test(t, func(t *testing.T) {
		db := &fakeDB{err: errors.New("connection refused")}
		hs := health.NewServer()
		logs, log := captureLogs()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			watchDatabase(ctx, db, hs, healthInterval, log)
			close(done)
		}()

		synctest.Wait()
		wantHealth(t, hs, healthpb.HealthCheckResponse_NOT_SERVING) // down at startup

		db.set(nil)
		time.Sleep(healthInterval)
		synctest.Wait()
		wantHealth(t, hs, healthpb.HealthCheckResponse_SERVING) // came up

		time.Sleep(3 * healthInterval) // three more pings, nothing changes
		synctest.Wait()

		db.set(errors.New("connection reset"))
		time.Sleep(healthInterval)
		synctest.Wait()
		wantHealth(t, hs, healthpb.HealthCheckResponse_NOT_SERVING) // went down again

		cancel()
		<-done // the watcher ends with its context

		down := logs.entries(t, "database unreachable; health is NOT_SERVING")
		up := logs.entries(t, "database reachable; health is SERVING")
		if len(down) != 2 || len(up) != 1 {
			t.Errorf("logged %d outages and %d recoveries, want one line per change: 2 and 1", len(down), len(up))
		}
	})
}

func TestHealthcheckExitCodes(t *testing.T) {
	addr, hs, _ := startServer(t, inventory.NewServer())
	getenv := env(map[string]string{envAddr: addr})
	var stdout, stderr bytes.Buffer

	// newServer starts as NOT_SERVING, before the watcher has seen the database
	if code := healthcheck(getenv, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "NOT_SERVING") {
		t.Errorf("while NOT_SERVING: exit %d, stderr %q; want exit 1 naming the status", code, stderr.String())
	}

	setHealth(hs, healthpb.HealthCheckResponse_SERVING)
	stdout.Reset()
	stderr.Reset()
	if code := healthcheck(getenv, &stdout, &stderr); code != 0 || stdout.String() != "SERVING\n" {
		t.Errorf("while SERVING: exit %d, stdout %q, stderr %q; want exit 0 and SERVING", code, stdout.String(), stderr.String())
	}
}

func TestHealthcheckFailsWhenNothingListens(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close() // a free port, with nothing behind it

	var stdout, stderr bytes.Buffer
	if code := healthcheck(env(map[string]string{envAddr: addr}), &stdout, &stderr); code != 1 {
		t.Errorf("exit %d with no server, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Unavailable") {
		t.Errorf("stderr %q, want the Unavailable error", stderr.String())
	}
}

func TestProbeTargetDialsLoopbackForWildcards(t *testing.T) {
	for listen, want := range map[string]string{
		":50051":          "127.0.0.1:50051",
		"0.0.0.0:50051":   "127.0.0.1:50051",
		"[::]:50051":      "127.0.0.1:50051",
		"127.0.0.1:6000":  "127.0.0.1:6000",
		"[::1]:50051":     "[::1]:50051",
		"inventory:50051": "inventory:50051",
	} {
		if got, err := probeTarget(listen); err != nil || got != want {
			t.Errorf("probeTarget(%q) = %q, %v; want %q", listen, got, err, want)
		}
	}

	if _, err := probeTarget("50051"); err == nil {
		t.Error("probeTarget accepted an address without a port")
	}
}

// wantHealth checks the status of the server as a whole and of the inventory service.
func wantHealth(t *testing.T, hs *health.Server, want healthpb.HealthCheckResponse_ServingStatus) {
	t.Helper()
	for _, service := range []string{"", inventoryv1.InventoryService_ServiceDesc.ServiceName} {
		resp, err := hs.Check(t.Context(), &healthpb.HealthCheckRequest{Service: service})
		if err != nil {
			t.Fatalf("Check(%q): %v", service, err)
		}
		if got := resp.GetStatus(); got != want {
			t.Errorf("Check(%q) = %v, want %v", service, got, want)
		}
	}
}
