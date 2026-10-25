//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"

	"koro/internal/platform/db"
)

const pgBatch = 3

// deleteBatch deletes at most pgBatch kv rows in ctx's transaction.
func deleteBatch(ctx context.Context, d *db.DB) (int, error) {
	res := db.Conn(ctx, d.ORM).Exec(`DELETE FROM kv WHERE id IN (SELECT id FROM kv ORDER BY id LIMIT ?)`, pgBatch)
	return int(res.RowsAffected), res.Error
}

func fill(t *testing.T, d *db.DB, n int) {
	t.Helper()
	for range n {
		if err := insert(t.Context(), d, "x"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestBatches_PG: on PostgreSQL every batch commits on its own; an empty
// table takes one empty batch, a table of exactly two batches a third,
// empty one, and a partial last batch ends the loop.
func TestBatches_PG(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		rows int
	}{{"empty", 0}, {"exact batch boundary", 2 * pgBatch}, {"partial last batch", 2*pgBatch + 1}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newKV(t)
			fill(t, d, tc.rows)
			tx := db.NewTxManager(d.ORM)
			got, err := db.Batches(t.Context(), tx, pgBatch, func(ctx context.Context) (int, error) {
				if !db.InTx(ctx) {
					t.Error("batch outside a transaction")
				}
				return deleteBatch(ctx, d)
			})
			if err != nil || got != tc.rows || count(t, d) != 0 {
				t.Fatalf("Batches = %d, %v, %d rows left; want %d, nil, 0", got, err, count(t, d), tc.rows)
			}
		})
	}
}

// TestBatches_PG_CancelMidBatch: the context ends while the second batch
// runs. Its transaction rolls back (COMMIT on an ended context), so its
// rows stay and are not counted; the first batch stays committed and no
// third batch starts.
func TestBatches_PG_CancelMidBatch(t *testing.T) {
	t.Parallel()
	d := newKV(t)
	fill(t, d, 3*pgBatch)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	got, err := db.Batches(ctx, db.NewTxManager(d.ORM), pgBatch, func(ctx context.Context) (int, error) {
		calls++
		n, err := deleteBatch(ctx, d)
		if calls == 2 {
			cancel()
		}
		return n, err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got != pgBatch || calls != 2 || count(t, d) != 2*pgBatch {
		t.Fatalf("Batches = %d after %d calls, %d rows left; want %d after 2, %d left", got, calls, count(t, d), pgBatch, 2*pgBatch)
	}
}
