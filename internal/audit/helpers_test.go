package audit_test

import (
	"context"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
)

// t0 is 2026-10-03 12:00 Europe/Istanbul.
var t0 = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

// day0 is the Europe/Istanbul day of t0.
var day0 = civil.Today(t0)

// testKeys is a fixed root signing keyring (test only).
var testKeys = []crypto.Key{{ID: "test1", Secret: []byte("koro-audit-test-root-seed-32byte")}}

type env struct {
	m       audit.Module
	svc     audit.Service
	st      audit.Store     // the store the module uses
	store   *audit.MemStore // the in-memory store (tamper helpers); unused by a PostgreSQL env
	clk     *clock.Fake
	archive *audit.MemArchive

	mu     sync.Mutex
	alarms []audit.Alarm
}

func (e *env) Alarms() []audit.Alarm {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]audit.Alarm(nil), e.alarms...)
}

// newEnv returns an audit module over the in-memory store with a fake
// clock at t0, a signer, an archive and an alarm recorder.
func newEnv(t *testing.T, mod func(d *audit.Deps)) *env {
	t.Helper()
	signer, err := audit.NewEd25519Signer(testKeys)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{store: audit.NewMemStore(), clk: clock.NewFake(t0), archive: audit.NewMemArchive()}
	d := audit.Deps{
		Store: e.store, Tx: db.NopTx{}, Clock: e.clk, Log: obs.Discard(), Signer: signer, Archive: e.archive,
		Alarm: func(_ context.Context, a audit.Alarm) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.alarms = append(e.alarms, a)
		},
	}
	if mod != nil {
		mod(&d)
	}
	e.st = d.Store
	e.m = audit.New(d)
	e.svc = e.m.Service
	return e
}

func ptr[T any](v T) *T { return &v }

func persona(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

// logN writes n events of action by user into org (nil: no org).
func (e *env) logN(t *testing.T, ctx context.Context, n int, action string, user uuid.UUID, org *uuid.UUID) {
	t.Helper()
	for i := range n {
		err := e.svc.Log(ctx, audit.Event{Action: action, Outcome: audit.Success, ActorUserID: &user, OrgID: org,
			ResourceType: "event", ResourceID: ptr(uuid.NewV7()), Metadata: map[string]any{"n": i}})
		if err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(time.Millisecond)
	}
}

func (e *env) chain(t *testing.T) int {
	t.Helper()
	n, err := audit.Chain(t.Context(), e.m)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	return n
}

// chained returns every chained row in chain order.
func (e *env) chained(t *testing.T) []audit.Record {
	t.Helper()
	rows, err := e.st.Chained(t.Context(), 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// window is a list window around t0.
func window() (time.Time, time.Time) { return t0.Add(-time.Hour), t0.Add(24 * time.Hour) }
