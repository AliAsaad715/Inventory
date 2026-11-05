package main

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	inventoryv1 "github.com/AliAsaad715/Shop/warehouse/gen/inventory/v1"
)

// inventoryMethods prefixes the full method name of every inventory call, and of nothing else.
var inventoryMethods = "/" + inventoryv1.InventoryService_ServiceDesc.ServiceName + "/"

// delayCalls holds each inventory call for d before its handler runs. It backs the demo-only
// INVENTORY_DELAY_MS, which makes the warehouse slow on purpose to show the app's deadlines: a
// call whose deadline passes while it waits ends with DEADLINE_EXCEEDED and never reaches its
// handler, so a delayed Reserve changes no stock. Health checks never wait, so the container
// stays healthy while it is slow.
func delayCalls(d time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !strings.HasPrefix(info.FullMethod, inventoryMethods) {
			return handler(ctx, req)
		}

		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
			return handler(ctx, req)
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
}
