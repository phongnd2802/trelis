package db_test

import (
	"context"
	"testing"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
)

func TestMigrateIsIdempotent(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()

	first, err := db.Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if first == 0 {
		t.Fatal("first Migrate applied 0 migrations on an empty database")
	}
	second, err := db.Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if second != 0 {
		t.Fatalf("second Migrate applied %d migrations, want 0", second)
	}
}

func TestMigrateEnablesSearchExtensions(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, pool); err != nil {
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
