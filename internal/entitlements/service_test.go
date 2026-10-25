package entitlements_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/entitlements"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// consumeConcurrently runs n consumptions of one unit with distinct refs
// at once and returns how many succeeded; every failure must be
// ErrNoEntitlement.
func consumeConcurrently(t *testing.T, svc entitlements.Service, org uuid.UUID, f entitlements.Feature, n int) int {
	t.Helper()
	var ok, other atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Go(func() {
			<-start
			ref := entitlements.Ref{Type: f.RefType(), ID: uuid.NewV7()}
			_, err := svc.Consume(context.Background(), org, f, 1, ref)
			switch {
			case err == nil:
				ok.Add(1)
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
	return int(ok.Load())
}

// TestConsume_ConcurrentLastUnit_OnlyOneSucceeds: N callers race for the
// last unit; exactly one wins and the quota is never exceeded (02 §10.1,
// 05 §13.6; the GORM variant runs in store_integration_test.go).
func TestConsume_ConcurrentLastUnit_OnlyOneSucceeds(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org := uuid.NewV7()
	id := e.quota(t, org, entitlements.FeatureTargetedPost, 3)
	for range 2 {
		if _, err := e.svc.Consume(t.Context(), org, entitlements.FeatureTargetedPost, 1, postRef()); err != nil {
			t.Fatal(err)
		}
	}
	if got := consumeConcurrently(t, e.svc, org, entitlements.FeatureTargetedPost, 32); got != 1 {
		t.Fatalf("%d consumptions of the last unit succeeded, want 1", got)
	}
	r := e.row(t, org, id)
	if r.QuantityUsed != 3 || r.Status != entitlements.StatusExhausted {
		t.Fatalf("after the race: used %d status %s, want 3 exhausted", r.QuantityUsed, r.Status)
	}
	p, err := e.store.ListUsages(t.Context(), org, entitlements.UsageFilter{}, nil, 100)
	if err != nil || len(p) != 3 {
		t.Fatalf("usages = %d (%v), want 3", len(p), err)
	}
}

// TestConsume_ConcurrentQuota_ExactlyKSucceed: 50 callers on 7 units spread
// over two rights → exactly 7 usages, both rights exhausted.
func TestConsume_ConcurrentQuota_ExactlyKSucceed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org := uuid.NewV7()
	a := e.quota(t, org, entitlements.FeatureContactRequest, 4)
	b := e.quota(t, org, entitlements.FeatureContactRequest, 3)
	if got := consumeConcurrently(t, e.svc, org, entitlements.FeatureContactRequest, 50); got != 7 {
		t.Fatalf("%d consumptions succeeded, want 7", got)
	}
	for _, id := range []uuid.UUID{a, b} {
		if r := e.row(t, org, id); r.Status != entitlements.StatusExhausted || r.Remaining() != 0 {
			t.Fatalf("right %s: status %s remaining %d", id, r.Status, r.Remaining())
		}
	}
}

func TestConsume_SameRefTwice_Idempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org := uuid.NewV7()
	id := e.quota(t, org, entitlements.FeatureContactRequest, 5)
	ref := contactRef()
	first, err := e.svc.Consume(t.Context(), org, entitlements.FeatureContactRequest, 1, ref)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.Consume(t.Context(), org, entitlements.FeatureContactRequest, 1, ref)
	if err != nil || again != first {
		t.Fatalf("second Consume = %s, %v; want %s, nil", again, err, first)
	}
	if r := e.row(t, org, id); r.QuantityUsed != 1 {
		t.Fatalf("quantity_used = %d after a repeated ref, want 1", r.QuantityUsed)
	}
	if us, _ := e.store.ListUsages(t.Context(), org, entitlements.UsageFilter{}, nil, 10); len(us) != 1 {
		t.Fatalf("%d usage rows, want 1", len(us))
	}
	// The same ref under another org is a caller bug, not a second usage.
	other := uuid.NewV7()
	e.quota(t, other, entitlements.FeatureContactRequest, 5)
	_, err = e.svc.Consume(t.Context(), other, entitlements.FeatureContactRequest, 1, ref)
	problemtest.WantErr(t, err, entitlements.ErrRefConflict)
}

// TestConsume_NoEntitlement_403Code: every way of lacking a right is 403
// entitlements.not_entitled (03 §3.3, T-58).
func TestConsume_NoEntitlement_403Code(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	now := e.clk.Now()
	check := func(name string, org uuid.UUID) {
		t.Helper()
		_, err := e.svc.Consume(t.Context(), org, entitlements.FeatureTargetedPost, 1, postRef())
		problemtest.WantErr(t, err, entitlements.ErrNoEntitlement)
		if status, code := problemtest.StatusCode(err); status != http.StatusForbidden || code != "entitlements.not_entitled" {
			t.Fatalf("%s: problem %d %q, want 403 entitlements.not_entitled", name, status, code)
		}
	}
	check("no right at all", uuid.NewV7())

	other := uuid.NewV7()
	e.quota(t, other, entitlements.FeatureTargetedPost, 5)
	check("another org's right", uuid.NewV7())

	wrongFeature := uuid.NewV7()
	e.quota(t, wrongFeature, entitlements.FeatureContactRequest, 5)
	check("another feature", wrongFeature)

	future := uuid.NewV7()
	e.grant(t, future, entitlements.FeatureTargetedPost, ptr(5), now.Add(time.Hour), now.Add(48*time.Hour))
	check("not started yet", future)

	ended := uuid.NewV7()
	e.grant(t, ended, entitlements.FeatureTargetedPost, ptr(5), now.Add(-48*time.Hour), now)
	check("ended (valid_to is exclusive)", ended)

	used := uuid.NewV7()
	e.quota(t, used, entitlements.FeatureTargetedPost, 1)
	if _, err := e.svc.Consume(t.Context(), used, entitlements.FeatureTargetedPost, 1, postRef()); err != nil {
		t.Fatal(err)
	}
	check("used up", used)

	pending := uuid.NewV7()
	line := uuid.NewV7()
	if _, err := e.svc.Grant(t.Context(), entitlements.GrantInput{
		OrgID: pending, Feature: entitlements.FeatureTargetedPost, Quantity: ptr(5), ValidFrom: now.Add(-time.Hour),
		ValidTo: now.Add(time.Hour), Status: entitlements.StatusPending, SourceType: entitlements.SourceOrderLine, SourceID: &line,
	}); err != nil {
		t.Fatal(err)
	}
	check("pending (unpaid order)", pending)

	revoked := uuid.NewV7()
	rid := e.quota(t, revoked, entitlements.FeatureTargetedPost, 5)
	if _, err := e.svc.AdminRevoke(t.Context(), actor(t, "platform_admin"), rid, "wrong org"); err != nil {
		t.Fatal(err)
	}
	check("revoked", revoked)

	// Two units on a right with one left is refused as a whole.
	one := uuid.NewV7()
	e.quota(t, one, entitlements.FeatureTargetedPost, 1)
	_, err := e.svc.Consume(t.Context(), one, entitlements.FeatureTargetedPost, 2, postRef())
	problemtest.WantErr(t, err, entitlements.ErrNoEntitlement)
}

func TestConsume_EarliestEndingRightFirstAndActorRecorded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org, now := uuid.NewV7(), e.clk.Now()
	late := e.grant(t, org, entitlements.FeatureTargetedDispatch, ptr(5), now.Add(-time.Hour), now.Add(60*24*time.Hour))
	early := e.grant(t, org, entitlements.FeatureTargetedDispatch, ptr(1), now.Add(-time.Hour), now.Add(24*time.Hour))
	cm := actor(t, "company_member")
	ctx := authz.WithActor(t.Context(), cm)
	ref := entitlements.Ref{Type: entitlements.RefDispatch, ID: uuid.NewV7()}
	if _, err := e.svc.Consume(ctx, org, entitlements.FeatureTargetedDispatch, 1, ref); err != nil {
		t.Fatal(err)
	}
	if e.row(t, org, early).QuantityUsed != 1 || e.row(t, org, late).QuantityUsed != 0 {
		t.Fatal("the right ending first was not consumed first")
	}
	u, _, err := e.store.UsageByRef(t.Context(), ref)
	if err != nil || u.ActorUserID == nil || *u.ActorUserID != cm.UserID || u.OrgID != org || u.EntitlementID != early {
		t.Fatalf("usage = %+v, %v", u, err)
	}
	// The next unit comes from the later right.
	if _, err := e.svc.Consume(t.Context(), org, entitlements.FeatureTargetedDispatch, 1,
		entitlements.Ref{Type: entitlements.RefDispatch, ID: uuid.NewV7()}); err != nil {
		t.Fatal(err)
	}
	if e.row(t, org, late).QuantityUsed != 1 {
		t.Fatal("the later right was not used after the first ran out")
	}
}

func TestConsume_ProgrammerErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org := uuid.NewV7()
	e.quota(t, org, entitlements.FeatureTargetedPost, 5)
	for name, call := range map[string]func() error{
		"nil org": func() error {
			_, err := e.svc.Consume(t.Context(), uuid.Nil(), entitlements.FeatureTargetedPost, 1, postRef())
			return err
		},
		"access feature": func() error {
			_, err := e.svc.Consume(t.Context(), org, entitlements.FeatureDiscoveryAccess, 1, postRef())
			return err
		},
		"zero quantity": func() error {
			_, err := e.svc.Consume(t.Context(), org, entitlements.FeatureTargetedPost, 0, postRef())
			return err
		},
		"ref of another feature": func() error {
			_, err := e.svc.Consume(t.Context(), org, entitlements.FeatureTargetedPost, 1, contactRef())
			return err
		},
	} {
		err := call()
		if err == nil || errors.Is(err, entitlements.ErrNoEntitlement) {
			t.Fatalf("%s: error = %v, want a programmer error", name, err)
		}
	}
}

func TestRelease_CreatesReversalRow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org := uuid.NewV7()
	id := e.quota(t, org, entitlements.FeatureTargetedPost, 1)
	ref := postRef()
	usageID, err := e.svc.Consume(t.Context(), org, entitlements.FeatureTargetedPost, 1, ref)
	if err != nil {
		t.Fatal(err)
	}
	if e.row(t, org, id).Status != entitlements.StatusExhausted {
		t.Fatal("the last unit did not exhaust the right")
	}
	mod := actor(t, "moderator")
	if err := e.svc.Release(authz.WithActor(t.Context(), mod), ref); err != nil {
		t.Fatal(err)
	}
	r := e.row(t, org, id)
	if r.QuantityUsed != 0 || r.Status != entitlements.StatusActive {
		t.Fatalf("after release: used %d status %s, want 0 active", r.QuantityUsed, r.Status)
	}
	rows, err := e.store.ListUsages(t.Context(), org, entitlements.UsageFilter{}, nil, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("usages = %+v, %v; want consumption + reversal", rows, err)
	}
	rev := rows[0] // newest first
	if rev.Quantity != -1 || rev.ReversalOf == nil || *rev.ReversalOf != usageID || rev.RefID != ref.ID ||
		rev.EntitlementID != id || rev.OrgID != org || rev.ActorUserID == nil || *rev.ActorUserID != mod.UserID {
		t.Fatalf("reversal row = %+v", rev)
	}
	// Releasing again, or a ref that consumed nothing, changes nothing.
	if err := e.svc.Release(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Release(t.Context(), postRef()); err != nil {
		t.Fatal(err)
	}
	if rows, _ := e.store.ListUsages(t.Context(), org, entitlements.UsageFilter{}, nil, 10); len(rows) != 2 {
		t.Fatalf("%d usage rows after repeated releases, want 2", len(rows))
	}
	// The released unit is usable again, but not by the released ref.
	_, err = e.svc.Consume(t.Context(), org, entitlements.FeatureTargetedPost, 1, ref)
	problemtest.WantErr(t, err, entitlements.ErrUsageReleased)
	if _, err := e.svc.Consume(t.Context(), org, entitlements.FeatureTargetedPost, 1, postRef()); err != nil {
		t.Fatalf("the released unit is not usable: %v", err)
	}
}

func TestRelease_AfterTheEndExpires(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org := uuid.NewV7()
	id := e.quota(t, org, entitlements.FeatureTargetedPost, 1)
	ref := postRef()
	if _, err := e.svc.Consume(t.Context(), org, entitlements.FeatureTargetedPost, 1, ref); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(31 * 24 * time.Hour)
	if err := e.svc.Release(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, org, id); r.Status != entitlements.StatusExpired || r.QuantityUsed != 0 {
		t.Fatalf("an ended exhausted right released: status %s used %d, want expired 0", r.Status, r.QuantityUsed)
	}
}

func TestExpire_MarksExpired(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org, now := uuid.NewV7(), e.clk.Now()
	ending := e.grant(t, org, entitlements.FeatureTargetedPost, ptr(5), now.Add(-time.Hour), now.Add(time.Hour))
	access := e.grant(t, org, entitlements.FeatureDiscoveryAccess, nil, now.Add(-time.Hour), now.Add(2*time.Hour))
	running := e.quota(t, org, entitlements.FeatureContactRequest, 5)
	exhausted := e.grant(t, org, entitlements.FeatureTargetedDispatch, ptr(1), now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := e.svc.Consume(t.Context(), org, entitlements.FeatureTargetedDispatch, 1,
		entitlements.Ref{Type: entitlements.RefDispatch, ID: uuid.NewV7()}); err != nil {
		t.Fatal(err)
	}
	jobs := e.mod.Jobs()
	if len(jobs) != 1 || jobs[0].Name != entitlements.ExpireJobName || jobs[0].Cron != "45 * * * *" || jobs[0].Queue != "default" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if err := jobs[0].Validate(); err != nil {
		t.Fatal(err)
	}

	e.clk.Advance(90 * time.Minute) // past ending, before access ends
	if err := jobs[0].Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID]entitlements.Status{
		ending: entitlements.StatusExpired, access: entitlements.StatusActive,
		running: entitlements.StatusActive, exhausted: entitlements.StatusExhausted,
	}
	for id, st := range want {
		if got := e.row(t, org, id).Status; got != st {
			t.Fatalf("right %s: status %s, want %s", id, got, st)
		}
	}
	ev := e.audit.All()
	if len(ev) != 1 || ev[0].Action != "entitlements.entitlement.expire" || ev[0].SystemJob != entitlements.ExpireJobName ||
		ev[0].ActorUserID != nil || *ev[0].ResourceID != ending || *ev[0].OrgID != org {
		t.Fatalf("audit events = %+v", ev)
	}
	// Idempotent: a second run changes nothing; access ends an hour later.
	if err := jobs[0].Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(e.audit.All()) != 1 {
		t.Fatal("a second run expired again")
	}
	e.clk.Advance(time.Hour)
	if err := jobs[0].Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := e.row(t, org, access).Status; got != entitlements.StatusExpired {
		t.Fatalf("access right status %s, want expired", got)
	}
	av, err := e.svc.Available(t.Context(), org, entitlements.FeatureDiscoveryAccess)
	if err != nil || av.Entitled() {
		t.Fatalf("Available after expiry = %+v, %v", av, err)
	}
}

func adminGrant(org string) (uuid.UUID, entitlements.AdminGrantInput) {
	return authztest.ID(org), entitlements.AdminGrantInput{
		Feature: entitlements.FeatureContactRequest, Kind: entitlements.KindQuota, Quantity: ptr(10),
		ValidFrom: t0.Add(-24 * time.Hour), ValidTo: t0.Add(365 * 24 * time.Hour),
		SourceType: entitlements.SourceAdminGrant, Reason: "Pilot package sold offline",
	}
}

func TestAdminGrant_IdempotencyKeyReplay(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	fin := actor(t, "finance_admin")
	org, in := adminGrant("company_a")
	ctx := idemCtx(t.Context(), fin, "grant-company-a-0001")
	first, err := e.svc.AdminGrant(ctx, fin, org, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != entitlements.StatusActive || first.GrantedBy == nil || *first.GrantedBy != fin.UserID ||
		first.ActivatedAt == nil || first.SourceID != nil || *first.QuantityTotal != 10 {
		t.Fatalf("granted = %+v", first)
	}
	again, err := e.svc.AdminGrant(ctx, fin, org, in)
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay = %s, %v; want %s", again.ID, err, first.ID)
	}
	rows, _ := e.store.List(t.Context(), org, entitlements.ListFilter{}, nil, 10)
	if len(rows) != 1 {
		t.Fatalf("%d rights after a replay, want 1", len(rows))
	}
	ev := e.audit.All()
	if len(ev) != 1 || ev[0].Action != "entitlements.entitlement.grant" || *ev[0].ActorUserID != fin.UserID ||
		ev[0].Reason != in.Reason || *ev[0].ResourceID != first.ID {
		t.Fatalf("audit events = %+v", ev)
	}
	// The same key with another body is refused (04 §1.8).
	in.Quantity = ptr(11)
	_, err = e.svc.AdminGrant(ctx, fin, org, in)
	problemtest.WantErr(t, err, idempotency.ErrKeyReused)
	// Without the key the same body grants again (a deliberate second package).
	in.Quantity = ptr(10)
	if second, err := e.svc.AdminGrant(authz.WithActor(t.Context(), fin), fin, org, in); err != nil || second.ID == first.ID {
		t.Fatalf("grant without a key = %s, %v", second.ID, err)
	}
}

func TestAdminGrant_RulesAndAuthz(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pad := actor(t, "platform_admin")
	org, base := adminGrant("company_b")
	for name, tc := range map[string]struct {
		mut  func(*entitlements.AdminGrantInput)
		want error
	}{
		"kind mismatch":       {func(in *entitlements.AdminGrantInput) { in.Kind = entitlements.KindAccess }, entitlements.ErrInvalidGrant},
		"quota without total": {func(in *entitlements.AdminGrantInput) { in.Quantity = nil }, entitlements.ErrInvalidGrant},
		"access with a total": {func(in *entitlements.AdminGrantInput) {
			in.Feature, in.Kind = entitlements.FeatureDiscoveryAccess, entitlements.KindAccess
		}, entitlements.ErrInvalidGrant},
		"already ended":     {func(in *entitlements.AdminGrantInput) { in.ValidTo = t0 }, entitlements.ErrInvalidGrant},
		"reversed window":   {func(in *entitlements.AdminGrantInput) { in.ValidFrom = in.ValidTo.Add(time.Hour) }, entitlements.ErrInvalidGrant},
		"order line source": {func(in *entitlements.AdminGrantInput) { in.SourceType = entitlements.SourceOrderLine }, entitlements.ErrInvalidGrant},
		"no reason":         {func(in *entitlements.AdminGrantInput) { in.Reason = "  " }, problem.ErrValidation},
		"too many units":    {func(in *entitlements.AdminGrantInput) { in.Quantity = ptr(100_001) }, entitlements.ErrInvalidGrant},
	} {
		in := base
		tc.mut(&in)
		_, err := e.svc.AdminGrant(t.Context(), pad, org, in)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: error = %v, want %v", name, err, tc.want)
		}
	}
	_, err := e.svc.AdminGrant(t.Context(), pad, uuid.Nil(), base)
	problemtest.WantErr(t, err, entitlements.ErrNotFound)
	// A company cannot grant itself rights; nothing is written.
	_, err = e.svc.AdminGrant(t.Context(), actor(t, "other_company_member"), org, base)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	if rows, _ := e.store.List(t.Context(), org, entitlements.ListFilter{}, nil, 10); len(rows) != 0 || len(e.audit.All()) != 0 {
		t.Fatalf("a refused grant wrote %d rights", len(rows))
	}
	// A promotion is its own source; access rights carry no total.
	promo := base
	promo.Feature, promo.Kind, promo.Quantity, promo.SourceType = entitlements.FeatureDiscoveryAccess, entitlements.KindAccess, nil, entitlements.SourcePromo
	got, err := e.svc.AdminGrant(t.Context(), pad, org, promo)
	if err != nil || got.SourceID == nil || got.Kind != entitlements.KindAccess || got.QuantityTotal != nil {
		t.Fatalf("promo grant = %+v, %v", got, err)
	}
	if av, _ := e.svc.Available(t.Context(), org, entitlements.FeatureDiscoveryAccess); !av.Entitled() || !av.AccessUntil.Equal(promo.ValidTo) {
		t.Fatalf("Available after the promo = %+v", av)
	}
}

func TestAdminRevoke(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pad := actor(t, "platform_admin")
	org := uuid.NewV7()
	id := e.quota(t, org, entitlements.FeatureContactRequest, 3)
	if _, err := e.svc.Consume(t.Context(), org, entitlements.FeatureContactRequest, 1, contactRef()); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.AdminRevoke(t.Context(), actor(t, "company_admin"), id, "not allowed")
	problemtest.WantErr(t, err, authz.ErrForbidden)

	got, err := e.svc.AdminRevoke(t.Context(), pad, id, "Granted to the wrong org")
	if err != nil || got.Status != entitlements.StatusRevoked || got.RevokedAt == nil || *got.RevokeReason != "Granted to the wrong org" || got.QuantityUsed != 1 {
		t.Fatalf("revoked = %+v, %v", got, err)
	}
	again, err := e.svc.AdminRevoke(t.Context(), pad, id, "again")
	if err != nil || again.Status != entitlements.StatusRevoked {
		t.Fatalf("second revoke = %+v, %v", again, err)
	}
	if ev := e.audit.All(); len(ev) != 1 || ev[0].Action != "entitlements.entitlement.revoke" || ev[0].Metadata["previous_status"] != "active" {
		t.Fatalf("audit events = %+v", ev)
	}
	_, err = e.svc.AdminRevoke(t.Context(), pad, uuid.NewV7(), "missing")
	problemtest.WantErr(t, err, entitlements.ErrNotFound)

	ended := e.grant(t, org, entitlements.FeatureTargetedPost, ptr(1), t0.Add(-time.Hour), t0.Add(time.Minute))
	e.clk.Advance(time.Hour)
	if err := e.mod.Jobs()[0].Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AdminRevoke(t.Context(), pad, ended, "too late")
	problemtest.WantErr(t, err, entitlements.ErrInvalidState)
}

func TestListUsages_ActorOnlyForPadFin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org := authztest.ID("company_a")
	e.quota(t, org, entitlements.FeatureContactRequest, 5)
	cm := actor(t, "company_member")
	if _, err := e.svc.Consume(authz.WithActor(t.Context(), cm), org, entitlements.FeatureContactRequest, 1, contactRef()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		persona string
		sees    bool
	}{{"company_member", false}, {"company_admin", false}, {"platform_admin", true}, {"finance_admin", true}} {
		p, err := e.svc.ListUsages(t.Context(), actor(t, tc.persona), org, entitlements.UsageQuery{})
		if err != nil || len(p.Items) != 1 {
			t.Fatalf("%s: %+v, %v", tc.persona, p, err)
		}
		got := p.Items[0].ActorUserID
		if tc.sees != (got != nil) || (got != nil && *got != cm.UserID) {
			t.Fatalf("%s: actor_user_id = %v, want visible=%v", tc.persona, got, tc.sees)
		}
		if p.Items[0].Feature != entitlements.FeatureContactRequest || p.Items[0].Quantity != 1 {
			t.Fatalf("%s: usage = %+v", tc.persona, p.Items[0])
		}
	}
	_, err := e.svc.ListUsages(t.Context(), actor(t, "other_company_member"), org, entitlements.UsageQuery{})
	problemtest.WantErr(t, err, authz.ErrNotFound)
	_, err = e.svc.ListUsages(t.Context(), actor(t, "dpo"), org, entitlements.UsageQuery{})
	problemtest.WantErr(t, err, authz.ErrForbidden)
}

func TestList_PagesAndAvailability(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org := authztest.ID("company_a")
	now := e.clk.Now()
	e.quota(t, org, entitlements.FeatureTargetedPost, 2)
	e.quota(t, org, entitlements.FeatureTargetedPost, 3)
	e.grant(t, org, entitlements.FeatureDiscoveryAccess, nil, now.Add(-time.Hour), now.Add(10*24*time.Hour))
	e.grant(t, org, entitlements.FeatureContactRequest, ptr(5), now.Add(time.Hour), now.Add(2*time.Hour)) // not started
	cm := actor(t, "company_member")
	p, err := e.svc.List(t.Context(), cm, org, entitlements.ListQuery{Limit: 3})
	if err != nil || len(p.Items) != 3 || p.Next == nil {
		t.Fatalf("first page = %d items next %v, %v", len(p.Items), p.Next, err)
	}
	p2, err := e.svc.List(t.Context(), cm, org, entitlements.ListQuery{Limit: 3, After: p.Next})
	if err != nil || len(p2.Items) != 1 || p2.Next != nil || p2.Items[0].ID.Compare(p.Items[2].ID) >= 0 {
		t.Fatalf("second page = %+v, %v", p2, err)
	}
	av := map[entitlements.Feature]entitlements.Availability{}
	for _, a := range p.Available {
		av[a.Feature] = a
	}
	if len(av) != 4 || av[entitlements.FeatureTargetedPost].Remaining != 5 || av[entitlements.FeatureContactRequest].Entitled() ||
		av[entitlements.FeatureTargetedDispatch].Entitled() || !av[entitlements.FeatureDiscoveryAccess].AccessUntil.Equal(now.Add(10*24*time.Hour)) {
		t.Fatalf("available = %+v", p.Available)
	}
	f, err := e.svc.List(t.Context(), cm, org, entitlements.ListQuery{Filter: entitlements.ListFilter{Feature: entitlements.FeatureTargetedPost, Status: entitlements.StatusActive}})
	if err != nil || len(f.Items) != 2 {
		t.Fatalf("filtered = %d, %v", len(f.Items), err)
	}
}

// TestGet_OtherOrgsEntitlementIsNotFound: the org comes from the path and
// scopes the read (06 H-38); another org's ID is 404 even for staff.
func TestGet_OtherOrgsEntitlementIsNotFound(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a, b := authztest.ID("company_a"), authztest.ID("company_b")
	ida := e.quota(t, a, entitlements.FeatureTargetedPost, 1)
	idb := e.quota(t, b, entitlements.FeatureTargetedPost, 1)
	if got, err := e.svc.Get(t.Context(), actor(t, "company_admin"), a, ida); err != nil || got.ID != ida {
		t.Fatalf("own entitlement = %+v, %v", got, err)
	}
	for _, p := range []string{"company_admin", "platform_admin"} {
		_, err := e.svc.Get(t.Context(), actor(t, p), a, idb)
		problemtest.WantErr(t, err, entitlements.ErrNotFound)
	}
	_, err := e.svc.Get(t.Context(), actor(t, "company_admin"), b, idb)
	problemtest.WantErr(t, err, authz.ErrNotFound) // not a member of company_b
}

// TestGrant_SourceLifecycle is the orders path (BE-040): a pending right
// per order line and feature, activated on payment, revoked on refund.
func TestGrant_SourceLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org, line, now := uuid.NewV7(), uuid.NewV7(), e.clk.Now()
	in := entitlements.GrantInput{
		OrgID: org, Feature: entitlements.FeatureContactRequest, Quantity: ptr(10), ValidFrom: now, ValidTo: now.Add(90 * 24 * time.Hour),
		Status: entitlements.StatusPending, SourceType: entitlements.SourceOrderLine, SourceID: &line,
	}
	id, err := e.svc.Grant(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := e.svc.Grant(t.Context(), in); err != nil || again != id {
		t.Fatalf("second grant of the same source = %s, %v; want %s", again, err, id)
	}
	other := in
	other.OrgID = uuid.NewV7()
	_, err = e.svc.Grant(t.Context(), other)
	problemtest.WantErr(t, err, problem.ErrConflict)
	dispatch := in
	dispatch.Feature = entitlements.FeatureTargetedDispatch
	if did, err := e.svc.Grant(t.Context(), dispatch); err != nil || did == id {
		t.Fatalf("another feature of the same line = %s, %v", did, err)
	}
	if _, err := e.svc.Consume(t.Context(), org, entitlements.FeatureContactRequest, 1, contactRef()); !errors.Is(err, entitlements.ErrNoEntitlement) {
		t.Fatalf("a pending right was consumable: %v", err)
	}
	if err := e.svc.ActivateBySource(t.Context(), entitlements.SourceOrderLine, line); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, org, id); r.Status != entitlements.StatusActive || r.ActivatedAt == nil {
		t.Fatalf("activated = %+v", r)
	}
	if _, err := e.svc.Consume(t.Context(), org, entitlements.FeatureContactRequest, 1, contactRef()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RevokeBySource(t.Context(), entitlements.SourceOrderLine, line, "refund"); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, org, id); r.Status != entitlements.StatusRevoked || r.QuantityUsed != 1 || *r.RevokeReason != "refund" {
		t.Fatalf("revoked = %+v", r)
	}
	// Validation of the contract input.
	bad := in
	bad.SourceID = nil
	_, err = e.svc.Grant(t.Context(), bad)
	problemtest.WantErr(t, err, entitlements.ErrInvalidGrant)
	bad = in
	bad.Status = entitlements.StatusExhausted
	_, err = e.svc.Grant(t.Context(), bad)
	problemtest.WantErr(t, err, entitlements.ErrInvalidGrant)
}

func TestNew_RequiresEveryDependency(t *testing.T) {
	t.Parallel()
	if _, err := entitlements.New(entitlements.Deps{}); err == nil {
		t.Fatal("New with no dependencies succeeded")
	}
}

// Compile-time check: the module's audit port is satisfied by audit.Logger.
var _ entitlements.AuditLogger = audit.Logger(nil)
