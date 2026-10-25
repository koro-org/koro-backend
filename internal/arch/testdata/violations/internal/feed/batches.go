package feed

import "context"

const purgeBatch = 100

type txm struct{}

func (txm) WithTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

// purge is a hand-written batch loop: one transaction per batch, ended by
// a short batch.
func purge(ctx context.Context, tx txm, del func(context.Context) int) {
	for {
		var n int
		_ = tx.WithTx(ctx, func(ctx context.Context) error {
			n = del(ctx)
			return nil
		})
		if n < purgeBatch {
			return
		}
	}
}

// walk pages through rows by key: not a batch loop.
func walk(ctx context.Context, tx txm, page func(context.Context, int) []int) {
	after := 0
	for {
		var rows []int
		_ = tx.WithTx(ctx, func(ctx context.Context) error {
			rows = page(ctx, after)
			return nil
		})
		if len(rows) < purgeBatch {
			return
		}
		after = rows[len(rows)-1]
	}
}
