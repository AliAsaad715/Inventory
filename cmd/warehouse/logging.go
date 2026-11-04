package main

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// logCalls writes one JSON line per call with its method, gRPC code and duration, plus
// the order_id or product_id it carries, so a call can be followed from the app's logs
// into the warehouse's. Health checks come every few seconds, so they log at debug level.
func logCalls(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := status.Code(err)

		attrs := []slog.Attr{
			slog.String("method", info.FullMethod),
			slog.String("code", code.String()),
			slog.Float64("duration_ms", milliseconds(time.Since(start))),
		}
		// Reserve and Release carry an order_id, GetStock a product_id
		if r, ok := req.(interface{ GetOrderId() string }); ok {
			attrs = append(attrs, slog.String("order_id", r.GetOrderId()))
		}
		if r, ok := req.(interface{ GetProductId() string }); ok {
			attrs = append(attrs, slog.String("product_id", r.GetProductId()))
		}
		if err != nil {
			attrs = append(attrs, slog.String("error", status.Convert(err).Message()))
		}

		level := levelFor(code)
		if info.FullMethod == healthpb.Health_Check_FullMethodName {
			level = slog.LevelDebug
		}
		log.LogAttrs(ctx, level, "grpc call", attrs...)
		return resp, err
	}
}

// levelFor follows go-grpc-middleware's defaults: the caller's own mistakes are info,
// timeouts and unavailable dependencies are warnings, and failures of ours are errors.
func levelFor(code codes.Code) slog.Level {
	switch code {
	case codes.OK, codes.Canceled, codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.Unauthenticated:
		return slog.LevelInfo
	case codes.DeadlineExceeded, codes.PermissionDenied, codes.ResourceExhausted, codes.FailedPrecondition,
		codes.Aborted, codes.OutOfRange, codes.Unavailable:
		return slog.LevelWarn
	default: // Unknown, Unimplemented, Internal, DataLoss
		return slog.LevelError
	}
}

// recoverPanics turns a panicking handler into an INTERNAL error for that call alone,
// instead of a crash that drops every call in flight; the stack goes to the log. Deferred
// rollbacks run while the panic unwinds, so no transaction is left open.
func recoverPanics(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if p := recover(); p != nil {
				log.ErrorContext(ctx, "handler panicked",
					"method", info.FullMethod, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

// milliseconds keeps microsecond precision; slog would print a time.Duration as nanoseconds.
func milliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
