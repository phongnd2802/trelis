package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/db/sqlc"
)

// newStore is a Store on a fresh, migrated database.
func newStore(t *testing.T) db.Store {
	t.Helper()
	return db.NewStore(dbtest.Migrated(t))
}

// exists reports whether a Principal named name is visible through ctx.
func exists(ctx context.Context, t *testing.T, s db.Store, name string) bool {
	t.Helper()
	_, err := s.Querier(ctx).GetPrincipalByName(ctx, name)
	if errors.Is(err, db.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("get principal %q: %v", name, err)
	}
	return true
}

func TestExecTxCommitsWhenFnSucceeds(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	err := s.ExecTx(ctx, func(ctx context.Context) error {
		_, err := s.Querier(ctx).CreatePrincipal(ctx, "alice")
		return err
	})
	if err != nil {
		t.Fatalf("ExecTx: %v", err)
	}
	if !exists(ctx, t, s, "alice") {
		t.Error("alice is missing after a committed transaction")
	}
}

func TestExecTxRollsBackWhenFnFails(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	boom := errors.New("boom")
	err := s.ExecTx(ctx, func(ctx context.Context) error {
		if _, err := s.Querier(ctx).CreatePrincipal(ctx, "alice"); err != nil {
			return err
		}
		if !exists(ctx, t, s, "alice") {
			t.Error("alice not visible inside her own transaction")
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("ExecTx = %v, want the fn's error", err)
	}
	if exists(ctx, t, s, "alice") {
		t.Error("alice survived a rolled-back transaction")
	}
}

func TestExecTxRollsBackAndRepanicsWhenFnPanics(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("recovered %v, want the fn's panic", r)
			}
		}()
		_ = s.ExecTx(ctx, func(ctx context.Context) error {
			if _, err := s.Querier(ctx).CreatePrincipal(ctx, "alice"); err != nil {
				t.Fatal(err)
			}
			panic("boom")
		})
	}()
	if exists(ctx, t, s, "alice") {
		t.Error("alice survived a transaction whose fn panicked")
	}
}

func TestANestedExecTxJoinsTheOuterTransaction(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	boom := errors.New("outer fails after inner succeeded")
	err := s.ExecTx(ctx, func(ctx context.Context) error {
		if err := s.ExecTx(ctx, func(ctx context.Context) error {
			_, err := s.Querier(ctx).CreatePrincipal(ctx, "alice")
			return err
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("ExecTx = %v, want the outer error", err)
	}
	if exists(ctx, t, s, "alice") {
		t.Error("the inner ExecTx committed on its own; it should have joined the outer transaction")
	}
}

func TestOutsideExecTxTheQuerierDoesNotSeeUncommittedRows(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	_ = s.ExecTx(ctx, func(txCtx context.Context) error {
		if _, err := s.Querier(txCtx).CreatePrincipal(txCtx, "alice"); err != nil {
			t.Fatal(err)
		}
		if exists(ctx, t, s, "alice") {
			t.Error("a context outside the transaction sees its uncommitted row")
		}
		return errors.New("roll back")
	})
}

func TestIsUniqueViolation(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	if _, err := s.Querier(ctx).CreatePrincipal(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Querier(ctx).CreatePrincipal(ctx, "alice")
	if !db.IsUniqueViolation(err) {
		t.Errorf("IsUniqueViolation(%v) = false for a duplicate name", err)
	}
	if db.IsUniqueViolation(errors.New("other")) || db.IsForeignKeyViolation(err) {
		t.Error("a unique violation is reported as something else, or vice versa")
	}
}

func TestIsForeignKeyViolation(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	err := s.Querier(ctx).GrantDocumentToPrincipal(ctx, danglingGrant())
	if !db.IsForeignKeyViolation(err) {
		t.Errorf("IsForeignKeyViolation(%v) = false for a grant on a missing Document", err)
	}
}

// danglingGrant grants a Document that does not exist to a Principal that does
// not exist either.
func danglingGrant() sqlc.GrantDocumentToPrincipalParams {
	return sqlc.GrantDocumentToPrincipalParams{DocumentID: uuid.New(), PrincipalID: uuid.New()}
}

func TestTwoStoresKeepTheirTransactionsApart(t *testing.T) {
	pool := dbtest.Migrated(t)
	a, b, ctx := db.NewStore(pool), db.NewStore(pool), context.Background()
	_ = a.ExecTx(ctx, func(txCtx context.Context) error {
		if _, err := a.Querier(txCtx).CreatePrincipal(txCtx, "alice"); err != nil {
			t.Fatal(err)
		}
		// b was not asked to run in a's transaction, so it uses the pool.
		if exists(txCtx, t, b, "alice") {
			t.Error("another store picked up a's transaction from the context")
		}
		return errors.New("roll back")
	})
}
