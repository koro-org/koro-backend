// Package dbtest holds db test doubles for service tests with in-memory
// stores (05 §13.2) and the guard of a split Store (CheckStoreViews, 05
// §2.2). It is imported only by tests.
package dbtest

import (
	"context"
	"sync"

	"koro/internal/platform/db"
)

type txKey struct{}

// SpyTx is db.NopTx that numbers its outermost transactions (1, 2, …) and
// marks each one in the context, so a test can tell which calls share a
// transaction (TxOf). Nested calls join the outer transaction. The marker
// is absent in AfterCommit hooks: they get the caller's context values,
// which never carried it. The zero value is ready to use.
type SpyTx struct {
	mu sync.Mutex
	n  int
}

// WithTx runs fn in a numbered transaction (or joins the caller's).
func (s *SpyTx) WithTx(ctx context.Context, fn func(context.Context) error) error {
	if ctx.Value(txKey{}) != nil {
		return fn(ctx)
	}
	s.mu.Lock()
	s.n++
	n := s.n
	s.mu.Unlock()
	return db.NopTx{}.WithTx(ctx, func(inner context.Context) error {
		return fn(context.WithValue(inner, txKey{}, n))
	})
}

// WithSerializableTx is WithTx.
func (s *SpyTx) WithSerializableTx(ctx context.Context, fn func(context.Context) error) error {
	return s.WithTx(ctx, fn)
}

// Count returns how many outermost transactions ran.
func (s *SpyTx) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// TxOf returns the number of the SpyTx transaction ctx runs in (0: none).
func TxOf(ctx context.Context) int {
	n, _ := ctx.Value(txKey{}).(int)
	return n
}
