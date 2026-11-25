// Package dbtest gives integration tests a throwaway Postgres database on the
// server named by TRELIS_TEST_POSTGRES_DSN. Tests that use it skip when the
// variable is unset.
package dbtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/phongnd2802/trelis/internal/db"
)

// Fresh creates an empty database and returns a pool on it. The database is
// dropped when the test ends.
func Fresh(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TRELIS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TRELIS_TEST_POSTGRES_DSN (e.g. from `docker compose up postgres`) to run")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })

	name := fmt.Sprintf("trelis_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Migrated is Fresh with every migration applied.
func Migrated(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool := Fresh(t)
	if _, err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
