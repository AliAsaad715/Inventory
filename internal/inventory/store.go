package inventory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Item struct {
	ProductID string
	Quantity  int32
}

type Shortage struct {
	ProductID string
	Requested int32
	Available int32
}

type Result struct {
	Reserved  bool
	Shortages []Shortage
}

// Store owns the warehouse SQL. Each write uses one transaction on one pooled
// connection; no stock state is kept in the Go process.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// GetStock treats a product absent from the warehouse as having no stock.
func (s *Store) GetStock(ctx context.Context, productID string) (int32, error) {
	return readAvailable(ctx, s.pool, productID)
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readAvailable(ctx context.Context, db rowQuerier, productID string) (int32, error) {
	var available int32
	err := db.QueryRow(ctx, `SELECT available FROM stock_levels WHERE product_id = $1`, productID).Scan(&available)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read available stock: %w", err)
	}
	return available, nil
}

// Reserve takes every line or none. The unique order row serializes retries and
// Release, while conditional updates serialize different orders for the last unit.
func (s *Store) Reserve(ctx context.Context, orderID string, items []Item) (Result, error) {
	items, err := normalizeItems(items)
	if err != nil {
		return Result{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin reserve: %w", err)
	}
	defer rollback(tx)

	tag, err := tx.Exec(ctx, `INSERT INTO reservations (order_id, status)
		VALUES ($1, 'reserved') ON CONFLICT (order_id) DO NOTHING`, orderID)
	if err != nil {
		return Result{}, fmt.Errorf("insert reservation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var state string
		if err := tx.QueryRow(ctx, `SELECT status FROM reservations WHERE order_id = $1 FOR UPDATE`, orderID).Scan(&state); err != nil {
			return Result{}, fmt.Errorf("read reservation: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Result{}, fmt.Errorf("commit reserve retry: %w", err)
		}
		return Result{Reserved: state == "reserved"}, nil
	}

	for _, item := range items {
		tag, err := tx.Exec(ctx, `UPDATE stock_levels SET available = available - $2
			WHERE product_id = $1 AND available >= $2`, item.ProductID, item.Quantity)
		if err != nil {
			return Result{}, fmt.Errorf("decrease stock: %w", err)
		}
		if tag.RowsAffected() == 0 {
			available, err := readAvailable(ctx, tx, item.ProductID)
			if err != nil {
				return Result{}, err
			}
			// The deferred rollback restores earlier lines and removes the order row,
			// so a rejected reservation can be tried again after stock is available.
			return Result{Shortages: []Shortage{{
				ProductID: item.ProductID, Requested: item.Quantity, Available: available,
			}}}, nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO reservation_lines (order_id, product_id, quantity)
			VALUES ($1, $2, $3)`, orderID, item.ProductID, item.Quantity); err != nil {
			return Result{}, fmt.Errorf("insert reservation line: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit reserve: %w", err)
	}
	return Result{Reserved: true}, nil
}

// Release leaves a tombstone even if Reserve has not arrived. A late Reserve
// cannot consume stock for an order the app has already abandoned.
func (s *Store) Release(ctx context.Context, orderID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin release: %w", err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `INSERT INTO reservations (order_id, status)
		VALUES ($1, 'released') ON CONFLICT (order_id) DO NOTHING`, orderID); err != nil {
		return fmt.Errorf("insert release tombstone: %w", err)
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT status FROM reservations WHERE order_id = $1 FOR UPDATE`, orderID).Scan(&state); err != nil {
		return fmt.Errorf("lock reservation: %w", err)
	}
	if state == "reserved" {
		rows, err := tx.Query(ctx, `SELECT product_id::text, quantity FROM reservation_lines
			WHERE order_id = $1 ORDER BY product_id`, orderID)
		if err != nil {
			return fmt.Errorf("read reservation lines: %w", err)
		}
		// Finish reading before issuing updates on this transaction's connection.
		items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) {
			var item Item
			err := row.Scan(&item.ProductID, &item.Quantity)
			return item, err
		})
		if err != nil {
			return fmt.Errorf("scan reservation lines: %w", err)
		}
		for _, item := range items {
			if _, err := tx.Exec(ctx, `UPDATE stock_levels SET available = available + $2
				WHERE product_id = $1`, item.ProductID, item.Quantity); err != nil {
				return fmt.Errorf("restore stock: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE reservations SET status = 'released', updated_at = now()
			WHERE order_id = $1`, orderID); err != nil {
			return fmt.Errorf("mark reservation released: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit release: %w", err)
	}
	return nil
}

func rollback(tx pgx.Tx) {
	// Cleanup must still run after the caller's deadline. pgx closes the connection
	// if rollback fails, so an open transaction never goes back into the pool.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx) // ErrTxClosed is expected after a successful commit.
}
