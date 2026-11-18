// Package postgres implements the repository interfaces on PostgreSQL with
// pgx. Enum columns are written through a text cast and read as text, so pgx
// never needs to know the enum types.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/repository"
)

// DB wraps a connection pool and implements repository.TxManager.
type DB struct {
	Pool *pgxpool.Pool
}

// Open creates a pool. It does not wait for the database to be reachable.
func Open(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

// Close closes the pool.
func (db *DB) Close() { db.Pool.Close() }

// Ping checks that the database answers.
func (db *DB) Ping(ctx context.Context) error { return db.Pool.Ping(ctx) }

type txKey struct{}

// querier is the subset of pgx shared by the pool and a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// WithTx implements repository.TxManager.
func (db *DB) WithTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx) // join the outer transaction
	}
	ctx, span := observability.Tracer().Start(ctx, "db.transaction")
	defer span.End()
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// q returns the transaction in ctx, or the pool.
func (db *DB) q(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return db.Pool
}

// tx returns the transaction in ctx, or ErrTxRequired.
func (db *DB) tx(ctx context.Context) (pgx.Tx, error) {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx, nil
	}
	return nil, repository.ErrTxRequired
}

// mapErr translates driver errors into domain errors.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", domain.ErrAlreadyExists, pgErr.ConstraintName)
		case "23503", "23514", "22001": // foreign key, check, string too long
			return fmt.Errorf("%w: %s", domain.ErrValidation, pgErr.Message)
		}
	}
	return err
}
