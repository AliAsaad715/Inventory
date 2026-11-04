# Inventory

The warehouse serves `inventory.v1.InventoryService` over gRPC and stores stock
and reservations in PostgreSQL.

## Run locally with Docker

Start Docker Desktop with the Linux container engine, then run these commands
from the folder containing `compose.yaml`:

```powershell
docker compose up --build -d --wait
docker compose ps
```

The gRPC endpoint is `localhost:50051`. Use a gRPC client to call it.
PostgreSQL is available to the warehouse at `inventory-db:5432` inside Compose.
The database runs `db/001_schema.sql` and `db/002_seed.sql` on its first startup
and keeps its data in the `inventory-data` volume across restarts.

Optional settings can be placed in a local `.env` file before starting:

```dotenv
INVENTORY_DB_PASSWORD=inventory_local
INVENTORY_GRPC_PORT=50051
INVENTORY_LOG_LEVEL=info
```

The defaults are for local development. Once the database volume is initialized,
changing the password setting also requires updating the database role's password.

View logs or stop the services:

```powershell
docker compose logs --tail 100 warehouse inventory-db
docker compose down
```

`docker compose down` keeps the database volume.

## Warehouse tests (R2-05)

With Docker running, Go 1.27.3 and a C compiler available for the race detector,
run from the repository root:

```powershell
go test -race -count=1 -timeout=5m ./...
```

`TestStorePostgres` starts a disposable PostgreSQL 17 container using
[Testcontainers for Go](https://golang.testcontainers.org/modules/postgres/).
It loads the same `db/001_schema.sql` and `db/002_seed.sql` used by Compose and
resets the test tables before each case. It never connects to the application's
database. Docker is required; unavailable Docker fails the integration tests.

The tests cover 20 simultaneous reservations for the last unit (one accepted,
19 rejected), repeat reservations for the same order, stock restored exactly
once by Release, and reservations refused after Release. They also check early
Release tombstones, complete rollback on a shortage, duplicate product lines,
and concurrent retries and releases.

To see just the PostgreSQL cases and the last-unit race result:

```powershell
go test -race -count=1 -timeout=5m -v -run '^TestStorePostgres$' ./internal/inventory
```
