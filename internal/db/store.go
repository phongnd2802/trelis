package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/phongnd2802/trelis/internal/db/sqlc"
)

// Store is how repositories reach Postgres (ADR-0007). A transaction travels
// in the context: a service runs a use case in ExecTx, and every repository
// call made with that context gets its queries from Querier(ctx) and so runs
// in the same transaction. Only services call ExecTx.
type Store interface {
	// Querier returns the sqlc queries bound to ctx's transaction when ctx
	// came from ExecTx, and to the connection pool otherwise.
	Querier(ctx context.Context) sqlc.Querier
	// ExecTx runs fn in a transaction, committing when fn returns nil and
	// rolling back when it returns an error or panics (the panic is then
	// re-raised). Inside an ExecTx it joins the outer transaction, so fn's
	// error or panic decides the outer one.
	ExecTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// NewStore returns the Postgres Store on pool.
func NewStore(pool *pgxpool.Pool) Store {
	return &pgStore{pool: pool, q: sqlc.New(pool)}
}

type pgStore struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

// txKey keys a store's transaction in a context; holding the store keeps two
// stores on different pools from picking up each other's transaction.
type txKey struct{ store *pgStore }

func (s *pgStore) tx(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{s}).(pgx.Tx)
	return tx, ok
}

func (s *pgStore) Querier(ctx context.Context) sqlc.Querier {
	if tx, ok := s.tx(ctx); ok {
		return s.q.WithTx(tx)
	}
	return s.q
}

func (s *pgStore) ExecTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if _, ok := s.tx(ctx); ok {
		return fn(ctx)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
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
	if err = fn(context.WithValue(ctx, txKey{s}, tx)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}

// ErrNotFound is what a :one query returns when no row matches. Repositories
// map it to their own not-found errors.
var ErrNotFound = pgx.ErrNoRows

// SQLSTATE codes for the constraint violations repositories map.
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
)

// IsUniqueViolation reports whether err is a unique constraint violation.
func IsUniqueViolation(err error) bool { return hasCode(err, uniqueViolation) }

// IsForeignKeyViolation reports whether err is a foreign key violation.
func IsForeignKeyViolation(err error) bool { return hasCode(err, foreignKeyViolation) }

func hasCode(err error, code string) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == code
}
