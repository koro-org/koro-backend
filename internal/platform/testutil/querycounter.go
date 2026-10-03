//go:build integration

package testutil

import (
	"context"
	"fmt"
	"sync/atomic"

	"gorm.io/gorm"
)

// QueryCounter counts the GORM statements (query, create, update, delete,
// row, raw/exec) run with a context it marked; list endpoints use it to
// catch N+1 queries (05 §6.4: "≤ 3 queries"):
//
//	qc := testutil.NewQueryCounter()
//	_, _ = store.List(qc.Context(t.Context()), orgID, q)
//	if qc.Count() > 3 { t.Fatalf("List ran %d queries", qc.Count()) }
//
// It works on databases from NewDB (the callback is installed there once,
// so counting never races with parallel tests). Raw pgx calls on d.Pool are
// not counted.
type QueryCounter struct{ n atomic.Int64 }

type counterKey struct{}

// NewQueryCounter returns a zero counter.
func NewQueryCounter() *QueryCounter { return &QueryCounter{} }

// Context returns ctx marked for this counter.
func (q *QueryCounter) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, counterKey{}, q)
}

// Count returns the number of statements counted so far.
func (q *QueryCounter) Count() int { return int(q.n.Load()) }

// Reset sets the count back to zero.
func (q *QueryCounter) Reset() { q.n.Store(0) }

const counterCallback = "testutil:query_counter"

// installQueryCounter registers the counting callback on every GORM
// processor of orm.
func installQueryCounter(orm *gorm.DB) error {
	count := func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Context == nil {
			return
		}
		if q, ok := tx.Statement.Context.Value(counterKey{}).(*QueryCounter); ok {
			q.n.Add(1)
		}
	}
	cb := orm.Callback()
	for _, err := range []error{
		cb.Query().After("gorm:query").Register(counterCallback, count),
		cb.Create().After("gorm:create").Register(counterCallback, count),
		cb.Update().After("gorm:update").Register(counterCallback, count),
		cb.Delete().After("gorm:delete").Register(counterCallback, count),
		cb.Row().After("gorm:row").Register(counterCallback, count),
		cb.Raw().After("gorm:raw").Register(counterCallback, count),
	} {
		if err != nil {
			return fmt.Errorf("install query counter: %w", err)
		}
	}
	return nil
}
