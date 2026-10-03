//go:build integration

package entitlements_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/entitlements"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var seq atomic.Int64

// pgWorld inserts the parent rows (organizations, users) as app_rw.
func pgWorld(d *db.DB) world {
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return world{
		org: func(t *testing.T) uuid.UUID {
			id, n := uuid.NewV7(), strconv.FormatInt(seq.Add(1), 10)
			exec(t, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'company', $2, $3)`, id, "Şirket "+n, "sirket-"+n)
			return id
		},
		user: func(t *testing.T) uuid.UUID {
			id := uuid.NewV7()
			exec(t, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
				VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, "u"+strconv.FormatInt(seq.Add(1), 10)+"@example.test")
			return id
		},
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (entitlements.Store, world) {
		d := testutil.NewDB(t)
		return entitlements.NewGormStore(d.ORM), pgWorld(d)
	})
}

// pgEnv is the entitlements module over PostgreSQL (READ COMMITTED
// transactions of the app_rw pool). The clock is real: the conditional
// UPDATE compares the window with the time of each call.
type pgEnv struct {
	*env
	d *db.DB
	w world
}

func newPgEnv(t *testing.T) *pgEnv {
	t.Helper()
	d := testutil.NewDB(t)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	e := newEnvWith(t, clk, entitlements.NewGormStore(d.ORM), d.Tx, idempotency.NewGormStore(d.ORM))
	pe := &pgEnv{env: e, d: d, w: pgWorld(d)}
	e.admin = pe.w.user(t)
	return pe
}

func (e *pgEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.d.Pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// consumeInCallerTx runs n consumptions at once, each in its own caller
// transaction (the 01 §6.3 shape: the caller opens WithTx and consumes in
// it), and returns how many committed.
func (e *pgEnv) consumeInCallerTx(t *testing.T, org uuid.UUID, f entitlements.Feature, n int, ref func() entitlements.Ref) (int, []uuid.UUID) {
	t.Helper()
	var ok, other atomic.Int64
	var mu sync.Mutex
	var ids []uuid.UUID
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Go(func() {
			<-start
			var id uuid.UUID
			err := e.d.Tx.WithTx(context.Background(), func(ctx context.Context) error {
				var err error
				id, err = e.svc.Consume(ctx, org, f, 1, ref())
				return err
			})
			switch {
			case err == nil:
				ok.Add(1)
				mu.Lock()
				ids = append(ids, id)
				mu.Unlock()
			case !errors.Is(err, entitlements.ErrNoEntitlement):
				other.Add(1)
				t.Errorf("Consume: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	if other.Load() > 0 {
		t.Fatalf("%d consumptions failed with an unexpected error", other.Load())
	}
	return int(ok.Load()), ids
}

// TestConsume_ConcurrentLastUnit_OnlyOneSucceeds_Gorm is the race of the
// last unit against PostgreSQL: the conditional UPDATE lets exactly one of
// N concurrent transactions through (02 §10.1, 05 §13.6).
func TestConsume_ConcurrentLastUnit_OnlyOneSucceeds_Gorm(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	org := e.w.org(t)
	id := e.quota(t, org, entitlements.FeatureTargetedPost, 3)
	for range 2 {
		if _, err := e.svc.Consume(t.Context(), org, entitlements.FeatureTargetedPost, 1, postRef()); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := e.consumeInCallerTx(t, org, entitlements.FeatureTargetedPost, 24, postRef); got != 1 {
		t.Fatalf("%d consumptions of the last unit committed, want 1", got)
	}
	r := e.row(t, org, id)
	if r.QuantityUsed != 3 || r.Status != entitlements.StatusExhausted {
		t.Fatalf("used %d status %s, want 3 exhausted", r.QuantityUsed, r.Status)
	}
	if n := e.count(t, `SELECT count(*) FROM entitlement_usages WHERE entitlement_id = $1`, id); n != 3 {
		t.Fatalf("%d usage rows, want 3", n)
	}
}

// TestConsume_ConcurrentQuota_ExactlyKSucceed_Gorm: N=40 transactions on
// K=7 units over two rights → exactly K commits and Σ usages = K.
func TestConsume_ConcurrentQuota_ExactlyKSucceed_Gorm(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	org := e.w.org(t)
	e.quota(t, org, entitlements.FeatureContactRequest, 4)
	e.quota(t, org, entitlements.FeatureContactRequest, 3)
	if got, _ := e.consumeInCallerTx(t, org, entitlements.FeatureContactRequest, 40, contactRef); got != 7 {
		t.Fatalf("%d consumptions committed, want 7", got)
	}
	if n := e.count(t, `SELECT coalesce(sum(quantity), 0) FROM entitlement_usages WHERE org_id = $1`, org); n != 7 {
		t.Fatalf("Σ usages = %d, want 7", n)
	}
	if n := e.count(t, `SELECT count(*) FROM entitlements WHERE org_id = $1 AND status = 'exhausted' AND quantity_used = quantity_total`, org); n != 2 {
		t.Fatalf("%d exhausted rights, want 2", n)
	}
}

// TestConsume_SameRefConcurrent_OneUsage_Gorm: concurrent consumptions of
// one ref all answer with the same usage; one unit is used.
func TestConsume_SameRefConcurrent_OneUsage_Gorm(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	org := e.w.org(t)
	id := e.quota(t, org, entitlements.FeatureTargetedPost, 5)
	ref := postRef()
	got, ids := e.consumeInCallerTx(t, org, entitlements.FeatureTargetedPost, 12, func() entitlements.Ref { return ref })
	if got != 12 {
		t.Fatalf("%d of 12 same-ref consumptions committed", got)
	}
	for _, u := range ids {
		if u != ids[0] {
			t.Fatalf("same ref answered with different usages: %v", ids)
		}
	}
	if r := e.row(t, org, id); r.QuantityUsed != 1 {
		t.Fatalf("quantity_used = %d, want 1", r.QuantityUsed)
	}
}

// TestConsume_CallerRollback_RestoresQuota_Gorm: the consumption is part of
// the caller's transaction (01 §6.3); a failing caller gives the unit back.
func TestConsume_CallerRollback_RestoresQuota_Gorm(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	org := e.w.org(t)
	id := e.quota(t, org, entitlements.FeatureContactRequest, 1)
	errCaller := errors.New("contact request insert failed")
	err := e.d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		if _, err := e.svc.Consume(ctx, org, entitlements.FeatureContactRequest, 1, contactRef()); err != nil {
			return err
		}
		return errCaller
	})
	if !errors.Is(err, errCaller) {
		t.Fatalf("WithTx = %v", err)
	}
	if r := e.row(t, org, id); r.QuantityUsed != 0 || r.Status != entitlements.StatusActive {
		t.Fatalf("after rollback: used %d status %s", r.QuantityUsed, r.Status)
	}
	if n := e.count(t, `SELECT count(*) FROM entitlement_usages WHERE org_id = $1`, org); n != 0 {
		t.Fatalf("%d usage rows survived the rollback", n)
	}
	if _, err := e.svc.Consume(t.Context(), org, entitlements.FeatureContactRequest, 1, contactRef()); err != nil {
		t.Fatalf("the unit is not usable after the rollback: %v", err)
	}
}

func TestRelease_CreatesReversalRow_Gorm(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	org, user := e.w.org(t), e.w.user(t)
	id := e.quota(t, org, entitlements.FeatureTargetedPost, 1)
	ref := postRef()
	ctx := authz.WithActor(t.Context(), authz.Actor{Kind: authz.KindUser, UserID: user})
	usage, err := e.svc.Consume(ctx, org, entitlements.FeatureTargetedPost, 1, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Release(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Release(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM entitlement_usages
		WHERE reversal_of = $1 AND quantity = -1 AND entitlement_id = $2 AND org_id = $3 AND actor_user_id = $4`, usage, id, org, user); n != 1 {
		t.Fatalf("%d reversal rows, want 1", n)
	}
	if r := e.row(t, org, id); r.QuantityUsed != 0 || r.Status != entitlements.StatusActive {
		t.Fatalf("after release: used %d status %s", r.QuantityUsed, r.Status)
	}
	// The ledger stays append-only (koro_forbid_mutation).
	if _, err := e.d.Pool.Exec(t.Context(), `DELETE FROM entitlement_usages WHERE id = $1`, usage); err == nil {
		t.Fatal("a usage row could be deleted")
	}
	_, err = e.svc.Consume(t.Context(), org, entitlements.FeatureTargetedPost, 1, ref)
	if !errors.Is(err, entitlements.ErrUsageReleased) {
		t.Fatalf("re-consuming a released ref: %v", err)
	}
}

// TestAdminGrant_IdempotencyKeyReplay_Gorm: the grant, its audit row and
// the key commit together (06 H-5); a replay neither grants nor audits again.
func TestAdminGrant_IdempotencyKeyReplay_Gorm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	clk := clock.NewFake(t0)
	am := audit.New(audit.Deps{Store: audit.NewGormStore(d.ORM), Tx: d.Tx, Clock: clk, Log: obs.Discard()})
	mod, err := entitlements.New(entitlements.Deps{
		Store: entitlements.NewGormStore(d.ORM), Tx: d.Tx, Audit: am.Service, Clock: clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()}),
	})
	if err != nil {
		t.Fatal(err)
	}
	org := w.org(t)
	fin := authz.Actor{Kind: authz.KindUser, UserID: w.user(t), PlatformRoles: []authz.PlatformRole{authz.PlatformFinanceAdmin}, MFAVerified: true}
	_, in := adminGrant("company_a")
	ctx := idemCtx(t.Context(), fin, "grant-pg-0001")
	first, err := mod.Service.AdminGrant(ctx, fin, org, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := mod.Service.AdminGrant(ctx, fin, org, in)
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay = %s, %v; want %s", again.ID, err, first.ID)
	}
	count := func(sql string, args ...any) int {
		var n int
		if err := d.Pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM entitlements WHERE org_id = $1`, org); n != 1 {
		t.Fatalf("%d rights after a replay, want 1", n)
	}
	if n := count(`SELECT count(*) FROM audit_events WHERE action = 'entitlements.entitlement.grant' AND resource_id = $1 AND org_id = $2`, first.ID, org); n != 1 {
		t.Fatalf("%d audit rows, want 1", n)
	}
	// An org that does not exist is 404 (entitlements_org_id_fkey), and the
	// failed attempt leaves no key behind.
	_, err = mod.Service.AdminGrant(idemCtx(t.Context(), fin, "grant-pg-0002"), fin, uuid.NewV7(), in)
	if !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("grant to a missing org: %v", err)
	}
}
