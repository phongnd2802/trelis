// Package postgres owns the Trelis database: the connection pool and the
// goose migrations embedded in the binary.
package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var embedded embed.FS

// Open connects a pool to dsn and pings it, so a bad address or password
// fails here rather than on the first query.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies every pending migration and returns how many ran. Applied
// migrations are recorded in goose_db_version, so running it again is a no-op.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	migrations, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return 0, err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations)
	if err != nil {
		return 0, fmt.Errorf("postgres: migrations: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return len(results), fmt.Errorf("postgres: migrate: %w", err)
	}
	return len(results), nil
}
