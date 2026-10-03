//go:build integration

package idempotency_test

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// Every backend test also runs against PostgreSQL; the business write is an
// outbox row in the caller's transaction, so atomicity is checked on real
// data (06 H-5).
var _ = addBackend(backend{"postgres", func(t *testing.T) (idempotency.Store, db.TxManager, businessWriter) {
	d := testutil.NewDB(t)
	return idempotency.NewGormStore(d.ORM), d.Tx, pgBusiness{d: d}
}})

type pgBusiness struct{ d *db.DB }

func (b pgBusiness) write(ctx context.Context) error {
	return db.Conn(ctx, b.d.ORM).Exec(`INSERT INTO outbox (topic, aggregate_type, aggregate_id, payload)
		VALUES ( 'test.created', 'test', uuidv7(), '{}' )`).Error
}

func (b pgBusiness) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := b.d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestIdempotency_ConcurrentSameKey_OneRun: two requests with the same key
// race; the second waits on the unique index until the first commits, then
// replays its result (05 §10.4).
func TestIdempotency_ConcurrentSameKey_OneRun(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	k := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clock.Real(), Log: obs.Discard()})
	biz := pgBusiness{d: d}
	ctx := keyCtx(t, "key-concurrent-1")
	h := hash(t, "same body")
	var runs atomic.Int32
	results := make([]order, 4)
	replays := make([]bool, 4)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			o, replayed, err := idempotency.Do(ctx, k, h, func(ctx context.Context) (order, error) {
				runs.Add(1)
				if err := biz.write(ctx); err != nil {
					return order{}, err
				}
				time.Sleep(300 * time.Millisecond) // hold the key while the others arrive
				return order{ID: "only-one", Total: 42}, nil
			})
			if err != nil {
				t.Error(err)
			}
			results[i], replays[i] = o, replayed
		})
	}
	wg.Wait()
	if runs.Load() != 1 || biz.count(t) != 1 {
		t.Fatalf("runs = %d, business rows = %d; want 1 and 1", runs.Load(), biz.count(t))
	}
	replayed := 0
	for i, o := range results {
		if o.ID != "only-one" || o.Total != 42 {
			t.Fatalf("result %d = %+v", i, o)
		}
		if replays[i] {
			replayed++
		}
	}
	if replayed != len(results)-1 {
		t.Fatalf("%d replays, want %d", replayed, len(results)-1)
	}
}
