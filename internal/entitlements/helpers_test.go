package entitlements_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/entitlements"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

// auditLog records the events an entitlements service writes.
type auditLog struct {
	mu     sync.Mutex
	events []audit.Event
}

func (l *auditLog) Log(_ context.Context, e audit.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return nil
}

func (l *auditLog) all() []audit.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]audit.Event(nil), l.events...)
}

// env is an entitlements module over a store (in memory by default).
type env struct {
	clk   *clock.Fake
	store entitlements.Store
	audit *auditLog
	admin uuid.UUID // granted_by of env.grant
	mod   entitlements.Module
	svc   entitlements.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	return newEnvWith(t, clk, entitlements.NewMemStore(clk.Now), db.NopTx{}, idempotency.NewMemStore())
}

func newEnvWith(t *testing.T, clk *clock.Fake, store entitlements.Store, tx db.TxManager, keys idempotency.Store) *env {
	t.Helper()
	e := &env{clk: clk, store: store, audit: &auditLog{}, admin: authztest.ID("platform_admin")}
	mod, err := entitlements.New(entitlements.Deps{
		Store: store, Tx: tx, Audit: e.audit, Clock: clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: tx, Store: keys, Clock: clk, Log: obs.Discard()}),
	})
	if err != nil {
		t.Fatalf("entitlements.New: %v", err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
}

func actor(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func ptr[T any](v T) *T { return &v }

// grant writes an active admin grant of qty units (nil: access) valid in
// [from, to) for org and returns its ID.
func (e *env) grant(t *testing.T, org uuid.UUID, f entitlements.Feature, qty *int, from, to time.Time) uuid.UUID {
	t.Helper()
	admin := e.admin
	id, err := e.svc.Grant(t.Context(), entitlements.GrantInput{
		OrgID: org, Feature: f, Quantity: qty, ValidFrom: from, ValidTo: to, Status: entitlements.StatusActive,
		SourceType: entitlements.SourceAdminGrant, GrantedBy: &admin,
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	return id
}

// quota grants qty units of f valid for 30 days around now.
func (e *env) quota(t *testing.T, org uuid.UUID, f entitlements.Feature, qty int) uuid.UUID {
	t.Helper()
	now := e.clk.Now()
	return e.grant(t, org, f, &qty, now.Add(-time.Hour), now.Add(30*24*time.Hour))
}

func (e *env) row(t *testing.T, org, id uuid.UUID) entitlements.Entitlement {
	t.Helper()
	r, err := e.store.Get(t.Context(), org, id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return r
}

func postRef() entitlements.Ref {
	return entitlements.Ref{Type: entitlements.RefPost, ID: uuid.NewV7()}
}

func contactRef() entitlements.Ref {
	return entitlements.Ref{Type: entitlements.RefContactRequest, ID: uuid.NewV7()}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

// problemOf returns the status and code of a problem error.
func problemOf(err error) (int, string) {
	if p, ok := errors.AsType[*problem.Error](err); ok {
		return p.Status(), p.Code()
	}
	return 0, ""
}

// idemCtx carries an Idempotency-Key as the HTTP middleware would.
func idemCtx(ctx context.Context, a authz.Actor, key string) context.Context {
	return idempotency.WithKey(authz.WithActor(ctx, a), idempotency.Key{
		Scope: a.UserID.String() + ":POST /admin/orgs/{org_id}/entitlements", Value: key, Status: 201,
	})
}
