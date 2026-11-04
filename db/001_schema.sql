-- The warehouse's own schema. The inventory-db container runs the files in this folder in name
-- order, once, when it first starts on an empty data volume; the warehouse tests load the same
-- files. Each file is one transaction, so a file that fails leaves nothing half-made.
BEGIN;

-- Units on hand that no order holds. The CHECK backs up Reserve's conditional update
-- (WHERE available >= quantity), so stock can never go below zero.
CREATE TABLE stock_levels (
    product_id uuid    PRIMARY KEY,
    available  integer NOT NULL,
    CONSTRAINT ck_stock_levels_available_nonnegative CHECK (available >= 0)
);

-- One row per order, keyed by the order_id the app sends: 'reserved', or 'released' once the
-- stock is given back. A released row stays as a tombstone, so a late Reserve for an order
-- that was already released is refused instead of holding stock for nothing.
CREATE TABLE reservations (
    order_id   uuid        PRIMARY KEY,
    status     text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ck_reservations_status CHECK (status IN ('reserved', 'released'))
);

-- What each reservation holds, so Release knows how much to give back to which product.
CREATE TABLE reservation_lines (
    order_id   uuid    NOT NULL REFERENCES reservations (order_id),
    product_id uuid    NOT NULL REFERENCES stock_levels (product_id),
    quantity   integer NOT NULL,
    PRIMARY KEY (order_id, product_id),
    CONSTRAINT ck_reservation_lines_quantity_positive CHECK (quantity > 0)
);

COMMIT;
