package inventory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const (
	lastUnitProduct = "019a2180-0000-7000-8000-000000000205"
	soldOutProduct  = "019a2180-0000-7000-8000-000000000206"
	unknownProduct  = "ffffffff-ffff-4fff-8fff-ffffffffffff"
)

// One disposable Postgres container serves these sequential subtests. Every
// subtest reloads the real seed, so it does not depend on an earlier test's stock.
func TestStorePostgres(t *testing.T) {
	schemaPath := filepath.Join("..", "..", "db", "001_schema.sql")
	seedPath := filepath.Join("..", "..", "db", "002_seed.sql")
	seed, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	pool := newPostgresPool(t, schemaPath, seedPath)

	t.Run("GetStockIncludesUnknownAndSoldOutProducts", func(t *testing.T) {
		store, ctx := resetStore(t, pool, seed)
		wantStock(t, ctx, store, testProduct, 25)
		wantStock(t, ctx, store, soldOutProduct, 0)
		wantStock(t, ctx, store, unknownProduct, 0)
	})

	t.Run("TwentyOrdersReserveTheLastUnitExactlyOnce", func(t *testing.T) {
		store, ctx := resetStore(t, pool, seed)
		orders := make([]string, 20)
		for i := range orders {
			orders[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		}
		attempts := reserveConcurrently(ctx, store, orders, []Item{{ProductID: lastUnitProduct, Quantity: 1}})
		accepted, rejected := 0, 0
		for _, attempt := range attempts {
			if attempt.err != nil {
				t.Errorf("Reserve(%s): %v", attempt.orderID, attempt.err)
				continue
			}
			if attempt.result.Reserved {
				accepted++
				if len(attempt.result.Shortages) != 0 {
					t.Errorf("accepted order %s has shortages: %+v", attempt.orderID, attempt.result.Shortages)
				}
			} else {
				rejected++
				want := []Shortage{{ProductID: lastUnitProduct, Requested: 1, Available: 0}}
				if !reflect.DeepEqual(attempt.result.Shortages, want) {
					t.Errorf("order %s shortages = %+v, want %+v", attempt.orderID, attempt.result.Shortages, want)
				}
			}
		}
		if accepted != 1 || rejected != 19 {
			t.Fatalf("20 simultaneous orders: accepted=%d, rejected=%d; want 1 and 19", accepted, rejected)
		}
		wantStock(t, ctx, store, lastUnitProduct, 0)
		wantReservationRows(t, ctx, pool, 1, 1)
		t.Log("20 simultaneous orders: 1 accepted, 19 rejected, available stock 0")
	})

	t.Run("SameOrderTwiceDeductsEveryLineOnce", func(t *testing.T) {
		store, ctx := resetStore(t, pool, seed)
		items := []Item{{ProductID: testProduct, Quantity: 2}, {ProductID: testOther, Quantity: 3}}
		reserveStock(t, ctx, store, testOrder, items...)
		reserveStock(t, ctx, store, testOrder, items...)
		wantStock(t, ctx, store, testProduct, 23)
		wantStock(t, ctx, store, testOther, 97)
		wantReservationRows(t, ctx, pool, 1, 2)
	})

	t.Run("ReleaseRestoresEveryLineOnlyOnce", func(t *testing.T) {
		store, ctx := resetStore(t, pool, seed)
		reserveStock(t, ctx, store, testOrder,
			Item{ProductID: testProduct, Quantity: 2}, Item{ProductID: testOther, Quantity: 3})
		for range 2 {
			if err := store.Release(ctx, testOrder); err != nil {
				t.Fatalf("Release: %v", err)
			}
			wantStock(t, ctx, store, testProduct, 25)
			wantStock(t, ctx, store, testOther, 100)
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT status FROM reservations WHERE order_id = $1`, testOrder).Scan(&state); err != nil {
			t.Fatalf("read reservation status: %v", err)
		}
		if state != "released" {
			t.Fatalf("reservation status = %q, want released", state)
		}
	})

	t.Run("ReserveAfterReleaseIsRefused", func(t *testing.T) {
		store, ctx := resetStore(t, pool, seed)
		items := []Item{{ProductID: testProduct, Quantity: 2}}
		reserveStock(t, ctx, store, testOrder, items...)
		if err := store.Release(ctx, testOrder); err != nil {
			t.Fatalf("Release: %v", err)
		}
		result, err := store.Reserve(ctx, testOrder, items)
		if err != nil || result.Reserved || len(result.Shortages) != 0 {
			t.Fatalf("Reserve after Release = %+v, %v; want normal rejection", result, err)
		}
		wantStock(t, ctx, store, testProduct, 25)
		wantReservationRows(t, ctx, pool, 1, 1)
	})

	t.Run("ReleaseBeforeReserveLeavesATombstone", func(t *testing.T) {
		store, ctx := resetStore(t, pool, seed)
		if err := store.Release(ctx, testOrder); err != nil {
			t.Fatalf("early Release: %v", err)
		}
		result, err := store.Reserve(ctx, testOrder, []Item{{ProductID: lastUnitProduct, Quantity: 1}})
		if err != nil || result.Reserved || len(result.Shortages) != 0 {
			t.Fatalf("late Reserve = %+v, %v; want normal rejection", result, err)
		}
		wantStock(t, ctx, store, lastUnitProduct, 1)
		wantReservationRows(t, ctx, pool, 1, 0)
	})

	t.Run("ShortageRollsBackEarlierLinesAndAllowsRetry", func(t *testing.T) {
		store, ctx := resetStore(t, pool, seed)
		// The first product sorts before the sold-out one and is updated first.
		items := []Item{{ProductID: testProduct, Quantity: 2}, {ProductID: soldOutProduct, Quantity: 1}}
		result, err := store.Reserve(ctx, testOrder, items)
		want := []Shortage{{ProductID: soldOutProduct, Requested: 1, Available: 0}}
		if err != nil || result.Reserved || !reflect.DeepEqual(result.Shortages, want) {
			t.Fatalf("short Reserve = %+v, %v; want rejection with %+v", result, err, want)
		}
		wantStock(t, ctx, store, testProduct, 25)
		wantReservationRows(t, ctx, pool, 0, 0)

		if _, err := pool.Exec(ctx, `UPDATE stock_levels SET available = 1 WHERE product_id = $1`, soldOutProduct); err != nil {
			t.Fatalf("restock test product: %v", err)
		}
		reserveStock(t, ctx, store, testOrder, items...)
		wantStock(t, ctx, store, testProduct, 23)
		wantStock(t, ctx, store, soldOutProduct, 0)
		wantReservationRows(t, ctx, pool, 1, 2)
	})

	t.Run("DuplicateProductsReserveAndReleaseTheirSum", func(t *testing.T) {
		store, ctx := resetStore(t, pool, seed)
		reserveStock(t, ctx, store, testOrder,
			Item{ProductID: testProduct, Quantity: 2}, Item{ProductID: testProduct, Quantity: 3})
		wantStock(t, ctx, store, testProduct, 20)
		wantReservationRows(t, ctx, pool, 1, 1)
		var quantity int32
		if err := pool.QueryRow(ctx, `SELECT quantity FROM reservation_lines WHERE order_id = $1`, testOrder).Scan(&quantity); err != nil {
			t.Fatalf("read merged line: %v", err)
		}
		if quantity != 5 {
			t.Fatalf("merged quantity = %d, want 5", quantity)
		}
		if err := store.Release(ctx, testOrder); err != nil {
			t.Fatalf("Release merged lines: %v", err)
		}
		wantStock(t, ctx, store, testProduct, 25)
	})

	t.Run("ConcurrentRetriesAndReleasesApplyOnce", func(t *testing.T) {
		store, ctx := resetStore(t, pool, seed)
		orders := make([]string, 20)
		for i := range orders {
			orders[i] = testOrder
		}
		items := []Item{{ProductID: testProduct, Quantity: 2}, {ProductID: testOther, Quantity: 3}}
		for _, attempt := range reserveConcurrently(ctx, store, orders, items) {
			if attempt.err != nil || !attempt.result.Reserved || len(attempt.result.Shortages) != 0 {
				t.Errorf("concurrent retry = %+v, %v; want accepted", attempt.result, attempt.err)
			}
		}
		wantStock(t, ctx, store, testProduct, 23)
		wantStock(t, ctx, store, testOther, 97)
		wantReservationRows(t, ctx, pool, 1, 2)

		start := make(chan struct{})
		errors := make(chan error, 20)
		for range 20 {
			go func() {
				<-start
				errors <- store.Release(ctx, testOrder)
			}()
		}
		close(start)
		for range 20 {
			if err := <-errors; err != nil {
				t.Errorf("concurrent Release: %v", err)
			}
		}
		wantStock(t, ctx, store, testProduct, 25)
		wantStock(t, ctx, store, testOther, 100)
	})
}

func newPostgresPool(t *testing.T, schemaPath, seedPath string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	// The image Shop's compose runs as inventory-db, so the tests prove the PostgreSQL that serves orders
	container, err := postgres.Run(ctx, "postgres:18",
		postgres.WithDatabase("warehouse_test"),
		postgres.WithUsername("warehouse_test"),
		postgres.WithPassword("warehouse_test"),
		postgres.WithInitScripts(schemaPath, seedPath),
		postgres.BasicWaitStrategies(),
	)
	if container != nil {
		testcontainers.CleanupContainer(t, container)
	}
	if err != nil {
		t.Fatalf("start test PostgreSQL (Docker must be running): %v", err)
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("test PostgreSQL connection string: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test PostgreSQL configuration: %v", err)
	}
	cfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("connect to test PostgreSQL: %v", err)
	}
	return pool
}

func resetStore(t *testing.T, pool *pgxpool.Pool, seed []byte) (*Store, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	if _, err := pool.Exec(ctx, `TRUNCATE reservation_lines, reservations, stock_levels`); err != nil {
		t.Fatalf("reset test tables: %v", err)
	}
	if _, err := pool.Exec(ctx, string(seed)); err != nil {
		t.Fatalf("reload seed: %v", err)
	}
	return NewStore(pool), ctx
}

type reserveAttempt struct {
	orderID string
	result  Result
	err     error
}

func reserveConcurrently(ctx context.Context, store *Store, orders []string, items []Item) []reserveAttempt {
	start := make(chan struct{})
	completed := make(chan reserveAttempt, len(orders))
	for _, orderID := range orders {
		go func(id string) {
			<-start
			result, err := store.Reserve(ctx, id, items)
			completed <- reserveAttempt{orderID: id, result: result, err: err}
		}(orderID)
	}
	close(start)
	results := make([]reserveAttempt, 0, len(orders))
	for range orders {
		results = append(results, <-completed)
	}
	return results
}

func reserveStock(t *testing.T, ctx context.Context, store *Store, orderID string, items ...Item) {
	t.Helper()
	result, err := store.Reserve(ctx, orderID, items)
	if err != nil || !result.Reserved || len(result.Shortages) != 0 {
		t.Fatalf("Reserve = %+v, %v; want accepted", result, err)
	}
}

func wantStock(t *testing.T, ctx context.Context, store *Store, productID string, want int32) {
	t.Helper()
	got, err := store.GetStock(ctx, productID)
	if err != nil || got != want {
		t.Fatalf("GetStock(%s) = %d, %v; want %d", productID, got, err, want)
	}
}

func wantReservationRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wantOrders, wantLines int) {
	t.Helper()
	var orders, lines int
	err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM reservations), (SELECT count(*) FROM reservation_lines)`).Scan(&orders, &lines)
	if err != nil {
		t.Fatalf("count reservation rows: %v", err)
	}
	if orders != wantOrders || lines != wantLines {
		t.Fatalf("reservation rows = %d orders, %d lines; want %d and %d", orders, lines, wantOrders, wantLines)
	}
}
