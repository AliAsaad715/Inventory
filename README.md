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
