//go:build integration

package idempotency_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/testutil"
)

// Ways the business write of budgetApp outlives its request.
const (
	fast         int32 = iota
	pastDeadline       // a statement still running when the request budget ends
	stmtTimeout        // the server-side statement_timeout fires (57014)
)

// budgetApp is the real write stack on PostgreSQL: httpx with a short
// request budget, the Idempotency-Key middleware, a service that calls
// idempotency.Do and writes an outbox row in the same transaction, then
// lingers as mode says.
func budgetApp(t *testing.T, budget time.Duration) (http.Handler, *db.DB, *atomic.Int32) {
	t.Helper()
	d := testutil.NewDB(t)
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: userResolver{id: uuid.NewV7()}, RequestTimeout: budget})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(idempotency.Middleware(app.API))
	k := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clock.Real(), Log: obs.Discard()})
	biz := pgBusiness{d: d}
	var mode atomic.Int32
	huma.Register(app.V1, op.Koro(huma.Operation{OperationID: "createThing", Method: http.MethodPost, Path: "/things",
		Tags: []string{"orders"}, DefaultStatus: http.StatusCreated},
		op.Meta{Action: "orders.order.create", Surface: op.SurfaceSelf, Idempotency: op.IdempotencyRequired}),
		func(ctx context.Context, in *createIn) (*createOut, error) {
			h, err := idempotency.Hash(in.Body)
			if err != nil {
				return nil, err
			}
			o, _, err := idempotency.Do(ctx, k, h, func(ctx context.Context) (order, error) {
				if err := biz.write(ctx); err != nil {
					return order{}, err
				}
				switch mode.Load() {
				case pastDeadline:
					if err := db.Conn(ctx, d.ORM).Exec(`SELECT pg_sleep(5)`).Error; err != nil {
						return order{}, err
					}
				case stmtTimeout:
					if err := db.Conn(ctx, d.ORM).Exec(`SET LOCAL statement_timeout = '50ms'`).Error; err != nil {
						return order{}, err
					}
					if err := db.Conn(ctx, d.ORM).Exec(`SELECT pg_sleep(5)`).Error; err != nil {
						return order{}, err
					}
				}
				return order{ID: uuid.NewV7().String(), Total: int64(in.Body.Qty) * 100}, nil
			})
			if err != nil {
				return nil, err
			}
			return &createOut{Body: o}, nil
		})
	return app.Handler(), d, &mode
}

func keys(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM idempotency_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestIdempotency_RequestBudgetExhausted_503RollbackRetrySafe (T-315,
// BE-071): a write whose request runs out of time — its last statement
// outlives the request budget, or the server's statement_timeout cancels
// it (57014) — answers 503 unavailable over HTTP, rolls the business row
// back and does not consume the Idempotency-Key: the client's retry with
// the same key runs the write once (201), and a further retry replays it.
func TestIdempotency_RequestBudgetExhausted_503RollbackRetrySafe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		mode int32
	}{{"request budget", pastDeadline}, {"statement timeout 57014", stmtTimeout}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, d, mode := budgetApp(t, 500*time.Millisecond)
			biz := pgBusiness{d: d}
			const key = "0192f5c4-budget-001"

			mode.Store(tc.mode)
			rec := post(t, h, key, `{"qty":3}`)
			var p struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || rec.Code != http.StatusServiceUnavailable || p.Code != "unavailable" {
				t.Fatalf("write past the budget = %d %s, want 503 unavailable", rec.Code, rec.Body)
			}
			if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
			}
			if n, k := biz.count(t), keys(t, d); n != 0 || k != 0 {
				t.Fatalf("after the 503: %d business rows, %d keys; want a full rollback", n, k)
			}

			mode.Store(fast)
			first := post(t, h, key, `{"qty":3}`)
			if first.Code != http.StatusCreated || first.Header().Get(idempotency.HeaderReplayed) != "" {
				t.Fatalf("retry with the same key = %d %v %s, want a fresh 201", first.Code, first.Header(), first.Body)
			}
			again := post(t, h, key, `{"qty":3}`)
			if again.Code != http.StatusCreated || again.Header().Get(idempotency.HeaderReplayed) != "true" || again.Body.String() != first.Body.String() {
				t.Fatalf("second retry = %d %v %s, want the replay of %s", again.Code, again.Header(), again.Body, first.Body)
			}
			if n, k := biz.count(t), keys(t, d); n != 1 || k != 1 {
				t.Fatalf("after the retries: %d business rows, %d keys; want 1 and 1", n, k)
			}
		})
	}
}
