package db

import (
	"context"
	"fmt"
)

// UntilEmpty is the size for a step whose short batch is not its last one
// (a purge that runs in phases, a store call that may skip rows): Batches
// and Drain then stop at the first call that handles no row.
const UntilEmpty = 1

// Transactor is what Batches needs of a TxManager: one transaction per
// call. TxFunc adapts a module's own transaction runner.
type Transactor interface {
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// TxFunc is a function used as a Transactor (talent's inTx, which also
// switches the transaction's role).
type TxFunc func(ctx context.Context, fn func(ctx context.Context) error) error

// WithTx calls f.
func (f TxFunc) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return f(ctx, fn)
}

// Batches is the batch loop of jobs and retention sweeps (BE-112): it calls
// fn, each call in a transaction of its own, until a call handles fewer
// than size rows, and returns the rows of the committed calls.
//
// size is the batch size fn asks its store for (at most size rows per
// call), so a short call was the last one. No call starts after ctx ended:
// the loop then returns the total so far and ctx.Err(). An error of fn or
// of the commit ends the loop at once and comes back unchanged; the rows of
// that call are not counted (its transaction rolled back). Inside an outer
// transaction the calls of a TxManager join it (WithTx), so they commit or
// roll back with it.
func Batches(ctx context.Context, tx Transactor, size int, fn func(ctx context.Context) (int, error)) (int, error) {
	return Drain(ctx, size, func(ctx context.Context) (int, error) {
		var n int
		err := tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			n, err = fn(ctx)
			return err
		})
		return n, err
	})
}

// Drain is Batches without the transactions: for a step that is a single
// statement (it commits on its own) or that draws its own transaction
// boundary.
func Drain(ctx context.Context, size int, fn func(ctx context.Context) (int, error)) (int, error) {
	if size <= 0 {
		return 0, fmt.Errorf("db: batch size %d is not positive", size)
	}
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := fn(ctx)
		if err != nil {
			return total, err
		}
		total += n
		if n < size {
			return total, nil
		}
	}
}
