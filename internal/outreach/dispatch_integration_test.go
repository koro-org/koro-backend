//go:build integration

package outreach_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/entitlements"
	"koro/internal/outreach"
	"koro/internal/platform/clock"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
)

// failingAudit fails the audit record of one action: the last write of the
// dispatch's transaction.
type failingAudit struct {
	fail string
	log  auditLog
}

func (a *failingAudit) Log(ctx context.Context, e audit.Event) error {
	if e.Action == a.fail {
		return errors.New("audit store down")
	}
	return a.log.Log(ctx, e)
}

// TestScheduleDispatch_Postgres_RollbackReleasesUnit (01 §6.3, 06 H-38):
// in PostgreSQL the targeted_dispatch unit, the dispatch row and the
// outbox event are one transaction — when its last write fails, no unit is
// spent and no row nor event remains; when it commits, the dispatch
// carries the usage. Cancelling gives the unit back in the cancel's
// transaction (a failing cancel keeps it spent). The composite FK keeps a
// dispatch to its post's org; the run writes only published estimates.
func TestScheduleDispatch_Postgres_RollbackReleasesUnit(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	cma := actor(t, "company_admin")
	if _, err := d.Pool.Exec(ctx, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'company', 'Şirket A', 'sirket-a')`, companyA); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
		VALUES ($1, 'active', 'cma@example.test', 'cma@example.test', 'Ada', 'Yılmaz', 'company')`, cma.UserID); err != nil {
		t.Fatal(err)
	}
	w := pgWorld(d)
	idem := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()})
	entsMod, err := entitlements.New(entitlements.Deps{Store: entitlements.NewGormStore(d.ORM), Tx: d.Tx, Audit: &auditLog{},
		Idempotency: idem, Clock: clk, Log: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	ents := entsMod.Service
	two := 2
	if _, err := ents.Grant(ctx, entitlements.GrantInput{OrgID: companyA, Feature: entitlements.FeatureTargetedDispatch,
		Quantity: &two, ValidFrom: clk.Now().Add(-time.Hour), ValidTo: clk.Now().Add(24 * time.Hour),
		Status: entitlements.StatusActive, SourceType: entitlements.SourceAdminGrant, GrantedBy: &cma.UserID}); err != nil {
		t.Fatal(err)
	}
	audits := &failingAudit{fail: string(authz.OutreachDispatchCreate)}
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: outbox.NewRegistry(), Queue: queue.NewMem(),
		Clock: clk, Log: obs.Discard()})
	e := newEnvWith(t, clk, outreach.NewGormStore(d.ORM), d.Tx, idempotency.NewGormStore(d.ORM), func(_ *env, dd *outreach.Deps) {
		dd.Entitlements, dd.Audit, dd.Outbox = ents, audits, ob
	})
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	left := func() int {
		t.Helper()
		av, err := ents.Available(ctx, companyA, entitlements.FeatureTargetedDispatch)
		if err != nil {
			t.Fatal(err)
		}
		return av.Remaining
	}
	state := func() (units, usages, dispatches, events int) {
		return left(), count(`SELECT count(*) FROM entitlement_usages WHERE org_id = $1`, companyA),
			count(`SELECT count(*) FROM outreach_dispatches WHERE org_id = $1`, companyA),
			count(`SELECT count(*) FROM outbox WHERE topic = 'outreach.dispatch_scheduled'`)
	}
	post := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
	post.ID = w.post(t, companyA, cma.UserID)
	e.posts.put(post)
	in := outreach.DispatchInput{Actor: cma, Post: post, Channels: []outreach.Channel{outreach.ChannelPush}}

	// The audit write fails after the unit, the row and the event: all of it rolls back.
	if _, err := e.svc.ScheduleDispatch(ctx, in); err == nil {
		t.Fatal("ScheduleDispatch succeeded with a failing audit")
	}
	if u, us, ds, ev := state(); u != 2 || us != 0 || ds != 0 || ev != 0 {
		t.Fatalf("after rollback: units %d, usages %d, dispatches %d, events %d", u, us, ds, ev)
	}

	// It commits: one unit, one usage row of the dispatch, the event.
	audits.fail = ""
	id, err := e.svc.ScheduleDispatch(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if u, us, ds, ev := state(); u != 1 || us != 1 || ds != 1 || ev != 1 {
		t.Fatalf("after commit: units %d, usages %d, dispatches %d, events %d", u, us, ds, ev)
	}
	if n := count(`SELECT count(*) FROM outreach_dispatches o JOIN entitlement_usages u ON u.id = o.entitlement_usage_id
		WHERE o.id = $1 AND u.ref_type = 'dispatch' AND u.ref_id = o.id`, id); n != 1 {
		t.Fatal("the dispatch does not carry its usage")
	}

	// A failing cancel keeps the unit spent; a committed one gives it back.
	audits.fail = string(authz.OutreachDispatchCancel)
	if _, err := e.svc.CancelDispatch(ctx, cma, companyA, id); err == nil {
		t.Fatal("CancelDispatch succeeded with a failing audit")
	}
	if left() != 1 {
		t.Fatalf("units after a failed cancel = %d", left())
	}
	audits.fail = ""
	if v, err := e.svc.CancelDispatch(ctx, cma, companyA, id); err != nil || v.Status != outreach.DispatchCancelled || left() != 2 {
		t.Fatalf("cancel = %v, %v, units %d", v.Status, err, left())
	}

	// 06 H-38: a dispatch of another org's post is refused by the composite FK.
	other := w.org(t)
	stray := outreach.Dispatch{ID: uuid.NewV7(), OrgID: other, PostID: w.post(t, companyA, cma.UserID),
		AudienceKeys: []string{"all"}, AudienceDimCount: 1, Channels: []string{"push"}, DeepLink: "/posts/x",
		Status: outreach.DispatchScheduled, CreatedBy: cma.UserID}
	if _, err := e.store.InsertDispatch(ctx, stray); !errors.Is(err, outreach.ErrNotFound) {
		t.Fatalf("dispatch of another org's post: %v", err)
	}

	// The run over PostgreSQL: claimed, the published estimates written (37 → 30).
	ids := e.audience.add(37, true, uniKey(uniA))
	e.users.add(false, ids...)
	e.consents.grant(consent.PurposeCommercialPush, ids...)
	post2 := post
	post2.ID = w.post(t, companyA, cma.UserID)
	e.posts.put(post2)
	in.Post = post2
	id2, err := e.svc.ScheduleDispatch(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.run(t, outreach.Dispatch{ID: id2, OrgID: companyA, PostID: post2.ID}); err != nil {
		t.Fatal(err)
	}
	var status string
	var eligible, sent *int
	if err := d.Pool.QueryRow(ctx, `SELECT status, eligible_count, sent_count FROM outreach_dispatches WHERE id = $1`, id2).
		Scan(&status, &eligible, &sent); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || eligible == nil || *eligible != 30 || sent == nil || *sent != 30 {
		t.Fatalf("stored %s %v %v: only published estimates (37 → 30)", status, deref(eligible), deref(sent))
	}
	if len(e.sender.to("push")) != 37 {
		t.Fatalf("pushed %d, want 37", len(e.sender.to("push")))
	}
}
