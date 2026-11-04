// Package inventory answers the inventory.v1.InventoryService calls from the warehouse's
// own database.
package inventory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	inventoryv1 "github.com/AliAsaad715/Shop/warehouse/gen/inventory/v1"
)

type stockStore interface {
	GetStock(context.Context, string) (int32, error)
	Reserve(context.Context, string, []Item) (Result, error)
	Release(context.Context, string) error
}

// Server validates RPC input and translates storage results into the shared contract.
type Server struct {
	inventoryv1.UnimplementedInventoryServiceServer
	store stockStore
}

// NewServer returns the service that main registers on the gRPC server.
func NewServer(store stockStore) *Server {
	return &Server{store: store}
}

func (s *Server) GetStock(ctx context.Context, req *inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
	productID, err := normalizeID(req.GetProductId(), "product_id")
	if err != nil {
		return nil, rpcError(ctx, "GetStock", err)
	}
	available, err := s.store.GetStock(ctx, productID)
	if err != nil {
		return nil, rpcError(ctx, "GetStock", err)
	}
	return &inventoryv1.GetStockResponse{Available: available}, nil
}

func (s *Server) Reserve(ctx context.Context, req *inventoryv1.ReserveRequest) (*inventoryv1.ReserveResponse, error) {
	orderID, err := normalizeID(req.GetOrderId(), "order_id")
	if err != nil {
		return nil, rpcError(ctx, "Reserve", err)
	}
	items := make([]Item, 0, len(req.GetItems()))
	for _, item := range req.GetItems() {
		items = append(items, Item{ProductID: item.GetProductId(), Quantity: item.GetQuantity()})
	}
	items, err = normalizeItems(items)
	if err != nil {
		return nil, rpcError(ctx, "Reserve", err)
	}
	result, err := s.store.Reserve(ctx, orderID, items)
	if err != nil {
		return nil, rpcError(ctx, "Reserve", err)
	}
	resp := &inventoryv1.ReserveResponse{Reserved: result.Reserved}
	for _, shortage := range result.Shortages {
		resp.Shortages = append(resp.Shortages, &inventoryv1.Shortage{
			ProductId: shortage.ProductID, Requested: shortage.Requested, Available: shortage.Available,
		})
	}
	return resp, nil
}

func (s *Server) Release(ctx context.Context, req *inventoryv1.ReleaseRequest) (*inventoryv1.ReleaseResponse, error) {
	orderID, err := normalizeID(req.GetOrderId(), "order_id")
	if err != nil {
		return nil, rpcError(ctx, "Release", err)
	}
	if err := s.store.Release(ctx, orderID); err != nil {
		return nil, rpcError(ctx, "Release", err)
	}
	return &inventoryv1.ReleaseResponse{}, nil
}

func rpcError(ctx context.Context, operation string, err error) error {
	var input *invalidInput
	if errors.As(err, &input) {
		return status.Error(codes.InvalidArgument, input.Error())
	}
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	// PostgreSQL outages can be a connection failure or a server-side shutdown.
	var connectionError *pgconn.ConnectError
	var networkError net.Error
	var databaseError *pgconn.PgError
	unavailable := errors.As(err, &connectionError) || errors.As(err, &networkError) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, pgconn.ErrConnClosed)
	if errors.As(err, &databaseError) {
		unavailable = strings.HasPrefix(databaseError.Code, "08") || strings.HasPrefix(databaseError.Code, "53") ||
			databaseError.Code == "57P01" || databaseError.Code == "57P02" || databaseError.Code == "57P03"
	}
	if unavailable {
		slog.WarnContext(ctx, "inventory database unavailable", "operation", operation, "error", err)
		return status.Error(codes.Unavailable, "inventory database unavailable")
	}
	slog.ErrorContext(ctx, "inventory operation failed", "operation", operation, "error", err)
	return status.Error(codes.Internal, "internal inventory error")
}
