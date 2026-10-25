package dbtest_test

import (
	"context"
	"testing"

	"koro/internal/platform/db"
	"koro/internal/platform/db/dbtest"
)

func TestSpyTx_NumbersOutermostTransactions(t *testing.T) {
	t.Parallel()
	var tx dbtest.SpyTx
	var got []int
	hook := -1
	for range 2 {
		err := tx.WithTx(t.Context(), func(ctx context.Context) error {
			got = append(got, dbtest.TxOf(ctx))
			db.AfterCommit(ctx, func(hc context.Context) { hook = dbtest.TxOf(hc) })
			return tx.WithSerializableTx(ctx, func(inner context.Context) error {
				got = append(got, dbtest.TxOf(inner))
				return nil
			})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if want := []int{1, 1, 2, 2}; len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("TxOf = %v, want %v", got, want)
	}
	if tx.Count() != 2 {
		t.Fatalf("Count = %d, want 2", tx.Count())
	}
	if hook != 0 {
		t.Fatalf("TxOf in an AfterCommit hook = %d, want 0", hook)
	}
	if dbtest.TxOf(t.Context()) != 0 {
		t.Fatal("TxOf outside a transaction is not 0")
	}
}
