package idempotency_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// backend is one Store under test: memory always, PostgreSQL under the
// integration tag (idempotency_integration_test.go). business, when set,
// writes a row in the caller's transaction (to prove atomicity).
type backend struct {
	name string
	open func(t *testing.T) (idempotency.Store, db.TxManager, businessWriter)
}

type businessWriter interface {
	write(ctx context.Context) error
	count(t *testing.T) int
}

var backends = []backend{{"mem", func(*testing.T) (idempotency.Store, db.TxManager, businessWriter) {
	return idempotency.NewMemStore(), db.NopTx{}, nil
}}}

func addBackend(b backend) bool {
	backends = append(backends, b)
	return true
}

type env struct {
	store    idempotency.Store
	tx       db.TxManager
	business businessWriter
	clock    *clock.Fake
	keeper   *idempotency.Keeper
}

func forEachBackend(t *testing.T, fn func(t *testing.T, e *env)) {
	t.Helper()
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			store, tx, bw := b.open(t)
			clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
			k := idempotency.New(idempotency.Deps{Tx: tx, Store: store, Clock: clk, Log: obs.Discard()})
			fn(t, &env{store: store, tx: tx, business: bw, clock: clk, keeper: k})
		})
	}
}

var errCapacityFull = problem.New(problem.Conflict, "idempotency_test.capacity_full", "Full")

type order struct {
	ID    string `json:"id"`
	Total int64  `json:"total"`
}

func keyCtx(t *testing.T, value string) context.Context {
	t.Helper()
	return idempotency.WithKey(t.Context(), idempotency.Key{Scope: "0192f5c4-0000-7000-8000-000000000001:POST /api/v1/orders",
		Value: value, Status: http.StatusCreated})
}

func hash(t *testing.T, v any) []byte {
	t.Helper()
	h, err := idempotency.Hash(v)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestIdempotency_SameKeySameBody_Replays(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		ctx := keyCtx(t, "key-same-body-1")
		var runs atomic.Int32
		create := func(ctx context.Context) (order, error) {
			runs.Add(1)
			if e.business != nil {
				if err := e.business.write(ctx); err != nil {
					return order{}, err
				}
			}
			return order{ID: uuid.NewV7().String(), Total: 1500}, nil
		}
		first, replayed, err := idempotency.Do(ctx, e.keeper, hash(t, map[string]any{"offering": "x", "qty": 1}), create)
		if err != nil || replayed {
			t.Fatalf("first Do() = %+v, %v, %v", first, replayed, err)
		}
		second, replayed, err := idempotency.Do(ctx, e.keeper, hash(t, map[string]any{"qty": 1, "offering": "x"}), create)
		if err != nil || !replayed || second != first {
			t.Fatalf("second Do() = %+v, replayed %v, %v; want the first result %+v replayed", second, replayed, err, first)
		}
		if runs.Load() != 1 {
			t.Fatalf("business function ran %d times", runs.Load())
		}
		if e.business != nil && e.business.count(t) != 1 {
			t.Fatalf("business rows = %d, want 1", e.business.count(t))
		}

		// Another actor (scope) with the same key value is independent.
		other := idempotency.WithKey(t.Context(), idempotency.Key{Scope: "someone-else:POST /api/v1/orders", Value: "key-same-body-1"})
		if _, replayed, err := idempotency.Do(other, e.keeper, hash(t, map[string]any{"qty": 1, "offering": "x"}), create); err != nil || replayed {
			t.Fatalf("other scope: replayed %v, %v", replayed, err)
		}

		// After the TTL the key is free again.
		e.clock.Advance(idempotency.DefaultTTL + time.Second)
		if _, replayed, err := idempotency.Do(ctx, e.keeper, hash(t, map[string]any{"qty": 2}), create); err != nil || replayed {
			t.Fatalf("after expiry: replayed %v, %v", replayed, err)
		}
	})
}

func TestIdempotency_SameKeyDifferentBody_422(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		ctx := keyCtx(t, "key-diff-body-1")
		create := func(context.Context) (order, error) { return order{ID: "o1", Total: 1}, nil }
		if _, _, err := idempotency.Do(ctx, e.keeper, hash(t, "body-a"), create); err != nil {
			t.Fatal(err)
		}
		_, _, err := idempotency.Do(ctx, e.keeper, hash(t, "body-b"), create)
		if !errors.Is(err, idempotency.ErrKeyReused) {
			t.Fatalf("Do(different body) = %v, want ErrKeyReused", err)
		}
		var pe *problem.Error
		if !errors.As(err, &pe) || pe.Status() != http.StatusUnprocessableEntity || pe.Code() != "idempotency.key_reused" {
			t.Fatalf("error = %v, want 422 idempotency.key_reused", err)
		}
	})
}

func TestIdempotency_RollbackDoesNotStoreKey(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		ctx := keyCtx(t, "key-rollback-1")
		boom := errCapacityFull
		var runs atomic.Int32
		failing := func(ctx context.Context) (order, error) {
			runs.Add(1)
			if e.business != nil {
				if err := e.business.write(ctx); err != nil {
					return order{}, err
				}
			}
			return order{}, boom
		}
		if _, _, err := idempotency.Do(ctx, e.keeper, hash(t, "a"), failing); !errors.Is(err, boom) {
			t.Fatalf("Do() = %v, want the business error unchanged", err)
		}
		if e.business != nil && e.business.count(t) != 0 {
			t.Fatal("the business write survived the rollback")
		}
		// Same key, same body: runs again (nothing was stored).
		ok := func(context.Context) (order, error) { runs.Add(1); return order{ID: "o2"}, nil }
		if got, replayed, err := idempotency.Do(ctx, e.keeper, hash(t, "a"), ok); err != nil || replayed || got.ID != "o2" {
			t.Fatalf("retry after rollback = %+v, %v, %v", got, replayed, err)
		}
		if runs.Load() != 2 {
			t.Fatalf("runs = %d, want 2", runs.Load())
		}
	})
}

func TestIdempotency_WithoutKeyRunsPlainly(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		var runs int
		fn := func(context.Context) (int, error) { runs++; return runs, nil }
		for range 2 {
			if _, replayed, err := idempotency.Do(t.Context(), e.keeper, nil, fn); err != nil || replayed {
				t.Fatal(replayed, err)
			}
		}
		if runs != 2 {
			t.Fatalf("runs = %d", runs)
		}
		if _, _, err := idempotency.Do(keyCtx(t, "key-bad-hash"), e.keeper, []byte("short"), fn); err == nil {
			t.Fatal("a non-SHA-256 hash was accepted")
		}
	})
}

func TestIdempotency_CleanupJob(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		fn := func(context.Context) (int, error) { return 1, nil }
		for _, k := range []string{"key-clean-1", "key-clean-2"} {
			if _, _, err := idempotency.Do(keyCtx(t, k), e.keeper, hash(t, k), fn); err != nil {
				t.Fatal(err)
			}
		}
		job := e.keeper.CleanupJob()
		if err := job.Validate(); err != nil || job.Name != "idempotency.cleanup" || job.Cron != "5 * * * *" {
			t.Fatalf("job = %+v, %v", job, err)
		}
		if err := job.Handle(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Not expired yet: replay still works.
		if _, replayed, _ := idempotency.Do(keyCtx(t, "key-clean-1"), e.keeper, hash(t, "key-clean-1"), fn); !replayed {
			t.Fatal("cleanup removed a live key")
		}
		e.clock.Advance(25 * time.Hour)
		if err := job.Handle(t.Context()); err != nil {
			t.Fatal(err)
		}
		if ms, ok := e.store.(*idempotency.MemStore); ok && ms.Len() != 0 {
			t.Fatalf("mem keys after cleanup = %d", ms.Len())
		}
	})
}

// --- middleware ---

type userResolver struct{ id uuid.UUID }

func (r userResolver) Resolve(ctx context.Context, _ httpx.Credential) (context.Context, error) {
	return authz.WithActor(ctx, authz.Actor{Kind: authz.KindUser, UserID: r.id}), nil
}

type createIn struct {
	idempotency.RequiredHeader
	Body struct {
		Qty int `json:"qty"`
	}
}

type createOut struct {
	Body order
}

func newApp(t *testing.T, policy op.Idempotency) (http.Handler, *atomic.Int32) {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: userResolver{id: uuid.NewV7()}})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(idempotency.Middleware(app.API))
	k := idempotency.New(idempotency.Deps{Tx: db.NopTx{}, Store: idempotency.NewMemStore(), Clock: clock.Real()})
	var runs atomic.Int32
	h := func(ctx context.Context, in *createIn) (*createOut, error) {
		h, err := idempotency.Hash(in.Body)
		if err != nil {
			return nil, err
		}
		o, _, err := idempotency.Do(ctx, k, h, func(context.Context) (order, error) {
			runs.Add(1)
			return order{ID: uuid.NewV7().String(), Total: int64(in.Body.Qty) * 100}, nil
		})
		if err != nil {
			return nil, err
		}
		return &createOut{Body: o}, nil
	}
	if policy == op.IdempotencyRequired {
		huma.Register(app.V1, op.Koro(huma.Operation{OperationID: "createThing", Method: http.MethodPost, Path: "/things",
			Tags: []string{"orders"}, DefaultStatus: http.StatusCreated},
			op.Meta{Action: "orders.order.create", Surface: op.SurfaceSelf, Idempotency: policy}), h)
	} else {
		type optIn struct {
			idempotency.OptionalHeader
			Body struct {
				Qty int `json:"qty"`
			}
		}
		huma.Register(app.V1, op.Koro(huma.Operation{OperationID: "createThing", Method: http.MethodPost, Path: "/things",
			Tags: []string{"orders"}, DefaultStatus: http.StatusCreated},
			op.Meta{Action: "orders.order.create", Surface: op.SurfaceSelf, Idempotency: policy}),
			func(ctx context.Context, in *optIn) (*createOut, error) {
				return h(ctx, &createIn{Body: in.Body})
			})
	}
	return app.Handler(), &runs
}

func post(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/things", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer persona")
	if key != "" {
		req.Header.Set(idempotency.HeaderKey, key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware_ParsesKeyAndReplays(t *testing.T) {
	t.Parallel()
	h, runs := newApp(t, op.IdempotencyRequired)

	missing := post(t, h, "", `{"qty":1}`)
	if missing.Code != http.StatusUnprocessableEntity || !strings.Contains(missing.Body.String(), `"location":"header.Idempotency-Key"`) {
		t.Fatalf("missing key: %d %s", missing.Code, missing.Body)
	}
	if bad := post(t, h, "short", `{"qty":1}`); bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("short key: %d %s", bad.Code, bad.Body)
	}
	if bad := post(t, h, "has space in it", `{"qty":1}`); bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid key: %d", bad.Code)
	}

	first := post(t, h, "0192f5c4-key-001", `{"qty":2}`)
	if first.Code != http.StatusCreated || first.Header().Get(idempotency.HeaderReplayed) != "" {
		t.Fatalf("first: %d %v %s", first.Code, first.Header(), first.Body)
	}
	again := post(t, h, "0192f5c4-key-001", `{ "qty": 2 }`)
	if again.Code != http.StatusCreated || again.Header().Get(idempotency.HeaderReplayed) != "true" {
		t.Fatalf("replay: %d %v", again.Code, again.Header())
	}
	var a, b order
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(again.Body.Bytes(), &b)
	if a != b || a.Total != 200 {
		t.Fatalf("replayed body %+v differs from %+v", b, a)
	}
	reused := post(t, h, "0192f5c4-key-001", `{"qty":3}`)
	if reused.Code != http.StatusUnprocessableEntity || !strings.Contains(reused.Body.String(), `"code":"idempotency.key_reused"`) {
		t.Fatalf("reused key: %d %s", reused.Code, reused.Body)
	}
	if runs.Load() != 1 {
		t.Fatalf("business ran %d times", runs.Load())
	}
}

func TestMiddleware_OptionalKey(t *testing.T) {
	t.Parallel()
	h, runs := newApp(t, op.IdempotencyOptional)
	for range 2 {
		if rec := post(t, h, "", `{"qty":1}`); rec.Code != http.StatusCreated {
			t.Fatalf("without key: %d %s", rec.Code, rec.Body)
		}
	}
	for range 2 {
		if rec := post(t, h, "optional-key-1", `{"qty":1}`); rec.Code != http.StatusCreated {
			t.Fatalf("with key: %d %s", rec.Code, rec.Body)
		}
	}
	if runs.Load() != 3 {
		t.Fatalf("runs = %d, want 3 (two without key, one with)", runs.Load())
	}
}
