package inventory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	inventoryv1 "github.com/AliAsaad715/Shop/warehouse/gen/inventory/v1"
)

const (
	testOrder   = "3f8e2a64-9b1c-4c2e-8f7d-5a6b7c8d9e0f"
	testProduct = "019a2180-0000-7000-8000-000000000201"
	testOther   = "019a2180-0000-7000-8000-000000000202"
)

// A nil callback panics if invalid input accidentally reaches storage.
type stubStore struct {
	getStock func(context.Context, string) (int32, error)
	reserve  func(context.Context, string, []Item) (Result, error)
	release  func(context.Context, string) error
}

func (s stubStore) GetStock(ctx context.Context, id string) (int32, error) {
	return s.getStock(ctx, id)
}

func (s stubStore) Reserve(ctx context.Context, id string, items []Item) (Result, error) {
	return s.reserve(ctx, id, items)
}

func (s stubStore) Release(ctx context.Context, id string) error { return s.release(ctx, id) }

func TestHandlersRejectBadInputBeforeStorage(t *testing.T) {
	srv := NewServer(stubStore{})
	for _, id := range []string{"", "not-a-uuid", testProduct[:35], "--" + testProduct[2:], " " + testProduct[1:], strings.ReplaceAll(testProduct, "-", "")} {
		t.Run("UUID/"+id, func(t *testing.T) {
			_, err := srv.GetStock(t.Context(), &inventoryv1.GetStockRequest{ProductId: id})
			wantCode(t, err, codes.InvalidArgument)
			_, err = srv.Release(t.Context(), &inventoryv1.ReleaseRequest{OrderId: id})
			wantCode(t, err, codes.InvalidArgument)
			_, err = srv.Reserve(t.Context(), &inventoryv1.ReserveRequest{OrderId: id, Items: []*inventoryv1.ReserveItem{{ProductId: testProduct, Quantity: 1}}})
			wantCode(t, err, codes.InvalidArgument)
		})
	}
	for _, tc := range []struct {
		name  string
		items []*inventoryv1.ReserveItem
	}{
		{name: "empty"},
		{name: "nil item", items: []*inventoryv1.ReserveItem{nil}},
		{name: "bad product", items: []*inventoryv1.ReserveItem{{ProductId: "p-1", Quantity: 1}}},
		{name: "zero quantity", items: []*inventoryv1.ReserveItem{{ProductId: testProduct}}},
		{name: "negative quantity", items: []*inventoryv1.ReserveItem{{ProductId: testProduct, Quantity: -1}}},
		{name: "merged overflow", items: []*inventoryv1.ReserveItem{{ProductId: testProduct, Quantity: math.MaxInt32}, {ProductId: testProduct, Quantity: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := srv.Reserve(t.Context(), &inventoryv1.ReserveRequest{OrderId: testOrder, Items: tc.items})
			wantCode(t, err, codes.InvalidArgument)
		})
	}
	_, err := srv.GetStock(t.Context(), nil)
	wantCode(t, err, codes.InvalidArgument)
	_, err = srv.Reserve(t.Context(), nil)
	wantCode(t, err, codes.InvalidArgument)
	_, err = srv.Release(t.Context(), nil)
	wantCode(t, err, codes.InvalidArgument)
}

func TestReserveNormalizesItemsAndPassesTheCallContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv := NewServer(stubStore{reserve: func(gotCtx context.Context, id string, items []Item) (Result, error) {
		want := []Item{{ProductID: testProduct, Quantity: 3}, {ProductID: testOther, Quantity: 4}}
		if gotCtx != ctx || id != testOrder || !reflect.DeepEqual(items, want) {
			t.Fatalf("store received context %v, id %q, items %+v; want caller context, %q, %+v", gotCtx == ctx, id, items, testOrder, want)
		}
		return Result{Reserved: true}, nil
	}})
	resp, err := srv.Reserve(ctx, &inventoryv1.ReserveRequest{
		OrderId: strings.ToUpper(testOrder),
		Items: []*inventoryv1.ReserveItem{
			{ProductId: testOther, Quantity: 4},
			{ProductId: testProduct, Quantity: 1},
			{ProductId: strings.ToUpper(testProduct), Quantity: 2},
		},
	})
	if err != nil || !resp.GetReserved() || len(resp.GetShortages()) != 0 {
		t.Fatalf("Reserve = %v, %v; want successful reservation", resp, err)
	}
}

func TestReserveShortageAndTombstoneAreNormalReplies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result Result
	}{
		{name: "short stock", result: Result{Shortages: []Shortage{{ProductID: testProduct, Requested: 3, Available: 1}}}},
		{name: "released order", result: Result{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(stubStore{reserve: func(context.Context, string, []Item) (Result, error) { return tc.result, nil }})
			resp, err := srv.Reserve(t.Context(), &inventoryv1.ReserveRequest{OrderId: testOrder, Items: []*inventoryv1.ReserveItem{{ProductId: testProduct, Quantity: 3}}})
			if err != nil || resp.GetReserved() || len(resp.GetShortages()) != len(tc.result.Shortages) {
				t.Fatalf("Reserve = %v, %v; want normal rejection %+v", resp, err, tc.result)
			}
			if len(tc.result.Shortages) > 0 {
				got := resp.Shortages[0]
				if got.GetProductId() != testProduct || got.GetRequested() != 3 || got.GetAvailable() != 1 {
					t.Fatalf("shortage = %v, want product %s with 3 requested and 1 available", got, testProduct)
				}
			}
		})
	}
}

func TestGetStockAndReleasePassIDsAndContext(t *testing.T) {
	ctx := t.Context()
	released := false
	srv := NewServer(stubStore{
		getStock: func(gotCtx context.Context, id string) (int32, error) {
			if gotCtx != ctx || id != testProduct {
				t.Fatal("GetStock did not pass the caller context and normalized product ID")
			}
			return 0, nil // An unknown product is a successful reply with zero stock.
		},
		release: func(gotCtx context.Context, id string) error {
			if gotCtx != ctx || id != testOrder {
				t.Fatal("Release did not pass the caller context and normalized order ID")
			}
			released = true
			return nil
		},
	})
	stock, err := srv.GetStock(ctx, &inventoryv1.GetStockRequest{ProductId: strings.ToUpper(testProduct)})
	if err != nil || stock.GetAvailable() != 0 {
		t.Fatalf("GetStock = %v, %v; want zero available", stock, err)
	}
	resp, err := srv.Release(ctx, &inventoryv1.ReleaseRequest{OrderId: strings.ToUpper(testOrder)})
	if err != nil || resp == nil || !released {
		t.Fatalf("Release = %v, %v, called %v; want an empty successful reply", resp, err, released)
	}
}

func TestHandlersMapStorageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code codes.Code
	}{
		{"cancelled", fmt.Errorf("query: %w", context.Canceled), codes.Canceled},
		{"deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), codes.DeadlineExceeded},
		{"network", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, codes.Unavailable},
		{"connection lost", fmt.Errorf("query: %w", io.ErrUnexpectedEOF), codes.Unavailable},
		{"connection closed", fmt.Errorf("query: %w", pgconn.ErrConnClosed), codes.Unavailable},
		{"shutdown", &pgconn.PgError{Code: "57P01", Message: "shutdown"}, codes.Unavailable},
		{"connections exhausted", &pgconn.PgError{Code: "53300"}, codes.Unavailable},
		{"SQL failure", &pgconn.PgError{Code: "42P01", Message: "private table name"}, codes.Internal},
		{"unexpected", errors.New("private implementation details"), codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(stubStore{
				getStock: func(context.Context, string) (int32, error) { return 0, tc.err },
				reserve:  func(context.Context, string, []Item) (Result, error) { return Result{}, tc.err },
				release:  func(context.Context, string) error { return tc.err },
			})
			_, err := srv.GetStock(t.Context(), &inventoryv1.GetStockRequest{ProductId: testProduct})
			wantCode(t, err, tc.code)
			_, err = srv.Reserve(t.Context(), &inventoryv1.ReserveRequest{OrderId: testOrder, Items: []*inventoryv1.ReserveItem{{ProductId: testProduct, Quantity: 1}}})
			wantCode(t, err, tc.code)
			_, err = srv.Release(t.Context(), &inventoryv1.ReleaseRequest{OrderId: testOrder})
			wantCode(t, err, tc.code)
			if strings.Contains(status.Convert(err).Message(), "private") {
				t.Fatalf("RPC exposed database details: %v", err)
			}
		})
	}
}

func TestDatabaseCancellationUsesTheCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := rpcError(ctx, "Reserve", errors.New("query connection closed after cancellation"))
	wantCode(t, err, codes.Canceled)
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("error = %v, want %s", err, want)
	}
}
