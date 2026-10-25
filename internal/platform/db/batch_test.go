package db_test

import (
	"context"
	"errors"
	"testing"

	"koro/internal/platform/db"
	"koro/internal/platform/db/dbtest"
)

// rows is a step over a pool of left rows: each call takes at most size.
func rows(left *int, size int) func(context.Context) (int, error) {
	return func(context.Context) (int, error) {
		n := min(*left, size)
		*left -= n
		return n, nil
	}
}

func TestBatches_StopsAtShortBatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		left, calls int
	}{
		{"empty", 0, 1},
		{"one short batch", 3, 1},
		{"exact batch boundary", 10, 3}, // 5, 5, then an empty call ends it
		{"partial last batch", 12, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var tx dbtest.SpyTx
			left, total := tc.left, tc.left
			got, err := db.Batches(t.Context(), &tx, 5, rows(&left, 5))
			if err != nil || got != total || left != 0 {
				t.Fatalf("Batches = %d, %v (left %d), want %d, nil (left 0)", got, err, left, total)
			}
			if tx.Count() != tc.calls {
				t.Errorf("transactions = %d, want %d", tx.Count(), tc.calls)
			}
		})
	}
}

func TestBatches_EachCallInItsOwnTransaction(t *testing.T) {
	t.Parallel()
	var tx dbtest.SpyTx
	var seen []int
	left := 7
	step := rows(&left, 3)
	_, err := db.Batches(t.Context(), &tx, 3, func(ctx context.Context) (int, error) {
		seen = append(seen, dbtest.TxOf(ctx))
		return step(ctx)
	})
	if err != nil || len(seen) != 3 || seen[0] != 1 || seen[1] != 2 || seen[2] != 3 {
		t.Fatalf("transactions per call = %v (%v), want [1 2 3]", seen, err)
	}
}

func TestBatches_CancelBetweenBatches(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	got, err := db.Batches(ctx, db.NopTx{}, 2, func(context.Context) (int, error) {
		calls++
		if calls == 2 {
			cancel() // the batch in flight finishes; no further batch starts
		}
		return 2, nil
	})
	if !errors.Is(err, context.Canceled) || got != 4 || calls != 2 {
		t.Fatalf("Batches = %d, %v after %d calls, want 4, context.Canceled after 2", got, err, calls)
	}
}

func TestBatches_CancelledBeforeStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := db.Batches(ctx, db.NopTx{}, 2, func(context.Context) (int, error) {
		t.Error("a batch started on an ended context")
		return 0, nil
	})
	if !errors.Is(err, context.Canceled) || got != 0 {
		t.Fatalf("Batches = %d, %v, want 0, context.Canceled", got, err)
	}
}

func TestBatches_ErrorStopsAndIsNotCounted(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	calls := 0
	got, err := db.Batches(t.Context(), db.NopTx{}, 2, func(context.Context) (int, error) {
		calls++
		if calls == 3 {
			return 2, boom // a failed call's rows rolled back
		}
		return 2, nil
	})
	if !errors.Is(err, boom) || got != 4 || calls != 3 {
		t.Fatalf("Batches = %d, %v after %d calls, want 4, boom after 3", got, err, calls)
	}
}

// failingCommit runs fn and then fails, as a commit-time error would.
type failingCommit struct{ db.NopTx }

var errCommit = errors.New("commit failed")

func (failingCommit) WithTx(ctx context.Context, fn func(context.Context) error) error {
	if err := fn(ctx); err != nil {
		return err
	}
	return errCommit
}

func TestBatches_CommitErrorNotCounted(t *testing.T) {
	t.Parallel()
	got, err := db.Batches(t.Context(), failingCommit{}, 2, func(context.Context) (int, error) { return 2, nil })
	if !errors.Is(err, errCommit) || got != 0 {
		t.Fatalf("Batches = %d, %v, want 0, errCommit", got, err)
	}
}

func TestBatches_UntilEmpty(t *testing.T) {
	t.Parallel()
	// A phased step: short batches until it runs dry.
	results := []int{3, 1, 2, 0}
	calls := 0
	got, err := db.Drain(t.Context(), db.UntilEmpty, func(context.Context) (int, error) {
		n := results[calls]
		calls++
		return n, nil
	})
	if err != nil || got != 6 || calls != 4 {
		t.Fatalf("Drain(UntilEmpty) = %d, %v after %d calls, want 6, nil after 4", got, err, calls)
	}
}

func TestDrain_NoTransaction(t *testing.T) {
	t.Parallel()
	left := 5
	step := rows(&left, 2)
	got, err := db.Drain(t.Context(), 2, func(ctx context.Context) (int, error) {
		if db.InTx(ctx) {
			t.Error("Drain opened a transaction")
		}
		return step(ctx)
	})
	if err != nil || got != 5 {
		t.Fatalf("Drain = %d, %v, want 5, nil", got, err)
	}
}

func TestBatches_RejectsNonPositiveSize(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, -1} {
		if _, err := db.Batches(t.Context(), db.NopTx{}, size, func(context.Context) (int, error) {
			t.Error("a batch ran with a non-positive size")
			return 0, nil
		}); err == nil {
			t.Errorf("Batches(size %d) = nil error", size)
		}
	}
}
