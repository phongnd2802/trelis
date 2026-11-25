// Package db owns the Trelis database connection and applies the goose
// migrations from the root migrations package.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/phongnd2802/trelis/migrations"
)

// Open connects a pool to dsn and pings it, so a bad address or password
// fails here rather than on the first query.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies every pending migration and returns how many ran. Applied
// migrations are recorded in goose_db_version, so running it again is a no-op.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		return 0, fmt.Errorf("db: migrations: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return len(results), fmt.Errorf("db: migrate: %w", err)
	}
	return len(results), nil
}
