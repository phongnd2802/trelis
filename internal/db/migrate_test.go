package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// freshDatabase creates an empty database on the server named by
// TRELIS_TEST_POSTGRES_DSN and returns a pool on it. The database is dropped
// when the test ends.
func freshDatabase(t *testing.T) *pgxpool.Pool {
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

func TestMigrateIsIdempotent(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()

	first, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if first == 0 {
		t.Fatal("first Migrate applied 0 migrations on an empty database")
	}
	second, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if second != 0 {
		t.Fatalf("second Migrate applied %d migrations, want 0", second)
	}
}

func TestMigrateEnablesSearchExtensions(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, ext := range []string{"vector", "pg_search"} {
		var version string
		err := pool.QueryRow(ctx, "SELECT extversion FROM pg_extension WHERE extname = $1", ext).Scan(&version)
		if err != nil {
			t.Errorf("extension %s not installed: %v", ext, err)
			continue
		}
		t.Logf("%s %s", ext, version)
	}
}
