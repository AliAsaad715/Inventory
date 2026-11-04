# Build from this folder: docker build -t inventory-warehouse .
# The context is this folder alone: gen/ is committed, so the build needs nothing from contracts/
FROM golang:1.27.3 AS build
WORKDIR /src

# Download modules from go.mod and go.sum alone, so this layer stays cached until a dependency changes
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# No cgo: a static binary needs no C library, and distroless/static has none
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/warehouse ./cmd/warehouse

# No shell, package manager or curl: only CA certificates, time zones and a non-root user
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/warehouse /warehouse

# uid 65532; Compose publishes the gRPC port on the host's loopback interface
USER nonroot:nonroot
EXPOSE 50051
# Exec form, since there is no shell; the binary asks its own gRPC health service
HEALTHCHECK --interval=5s --timeout=3s --start-period=30s --start-interval=1s --retries=3 \
    CMD ["/warehouse", "healthcheck"]
ENTRYPOINT ["/warehouse"]
