//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// BE-071 (T-315): when the request budget (httpx, 05 §10.6) runs out in a
// write path, the transaction leaves nothing behind and the error the
// service returns still says why, so that httpx answers 503 unavailable
// instead of a 500. These tests pin the driver behaviour httpx relies on.

// budget is a request context whose budget is d.
func budget(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}

// TestWithTx_StatementPastRequestDeadline_RollsBack: a statement still
// running when the request context expires fails with an error that wraps
// context.DeadlineExceeded (pgx sets the connection deadline, it never
// asks the server to cancel), and the earlier write of the transaction is
// rolled back.
func TestWithTx_StatementPastRequestDeadline_RollsBack(t *testing.T) {
	t.Parallel()
	d := newKV(t)
	err := d.Tx.WithTx(budget(t, 300*time.Millisecond), func(ctx context.Context) error {
		if err := insert(ctx, d, "partial"); err != nil {
			return err
		}
		return db.Conn(ctx, d.ORM).Exec(`SELECT pg_sleep(5)`).Error
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WithTx() = %v, want an error wrapping context.DeadlineExceeded", err)
	}
	if _, ok := errors.AsType[*problem.Error](db.Translate(err)); ok {
		t.Fatalf("Translate(%v) became a problem; it must stay unchanged for httpx", err)
	}
	if n := count(t, d); n != 0 {
		t.Fatalf("%d rows survived the expired transaction", n)
	}
}

// TestWithTx_DeadlineBeforeCommit_RollsBackAsDeadline: every statement
// succeeded but the budget ran out before COMMIT. database/sql then rolls
// the transaction back itself and Commit may report only sql.ErrTxDone;
// WithTx keeps the context error in the chain (BE-071), so the caller
// still sees context.DeadlineExceeded. Repeat with -count to hit both
// orders of database/sql's rollback goroutine and Commit.
func TestWithTx_DeadlineBeforeCommit_RollsBackAsDeadline(t *testing.T) {
	t.Parallel()
	d := newKV(t)
	for range 5 {
		ctx := budget(t, 100*time.Millisecond)
		err := d.Tx.WithTx(ctx, func(ctx context.Context) error {
			if err := insert(ctx, d, "almost"); err != nil {
				return err
			}
			<-ctx.Done() // slow business logic between the last statement and COMMIT
			return nil
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WithTx() = %v, want an error wrapping context.DeadlineExceeded", err)
		}
	}
	if n := count(t, d); n != 0 {
		t.Fatalf("%d rows committed after the request budget ran out", n)
	}
}

// TestWithTx_StatementTimeout_57014RollsBack: the server-side
// statement_timeout (05 §6.1) cancels the statement with SQLSTATE 57014
// (query_canceled). Translate leaves it unchanged (05 §6.6) and httpx maps
// it to 503 unavailable; the transaction is rolled back.
func TestWithTx_StatementTimeout_57014RollsBack(t *testing.T) {
	t.Parallel()
	d := newKV(t)
	err := d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, d, "partial"); err != nil {
			return err
		}
		if err := db.Conn(ctx, d.ORM).Exec(`SET LOCAL statement_timeout = '50ms'`).Error; err != nil {
			return err
		}
		return db.Conn(ctx, d.ORM).Exec(`SELECT pg_sleep(5)`).Error
	})
	if db.SQLState(err) != "57014" {
		t.Fatalf("WithTx() = %v (SQLSTATE %q), want 57014", err, db.SQLState(err))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a server-side timeout must not look like the request deadline: %v", err)
	}
	if !errors.Is(db.Translate(err), err) {
		t.Fatalf("Translate changed a 57014 error: %v", db.Translate(err))
	}
	if n := count(t, d); n != 0 {
		t.Fatalf("%d rows survived the cancelled transaction", n)
	}
}
