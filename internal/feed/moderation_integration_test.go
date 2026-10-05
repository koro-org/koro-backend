//go:build integration

package feed_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/entitlements"
	"koro/internal/feed"
	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
)

// fixedTargeting compiles every spec into one uni key with an estimate of
// 40 (outreach's own rules are tested in outreach and above).
type fixedTargeting struct{ key string }

func (f fixedTargeting) Compile(_ context.Context, _ authz.Actor, spec outreach.SegmentSpec) (outreach.Compiled, error) {
	return outreach.Compiled{Keys: []string{f.key}, DimCount: 1, Definition: *spec.Definition, Size: 40}, nil
}

// failingAudit fails the audit record of one action (the last write of the
// targeted post's transaction).
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

// TestTargetedPost_ConsumeAndPostSameTx_RollbackReleases (01 §6.3): the
// compiled audience, the targeted_post unit and the post are one
// transaction in PostgreSQL. When a later write of it fails (the audit
// record), the unit comes back and no usage row nor post remains; when it
// commits, the post carries the usage; deleting the draft writes the
// reversal row and gives the unit back in the delete's transaction.
func TestTargetedPost_ConsumeAndPostSameTx_RollbackReleases(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	ctx := t.Context()
	company, author, admin := w.org(t, feed.AuthorCompany), w.user(t), w.user(t)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	idem := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()})
	entsMod, err := entitlements.New(entitlements.Deps{Store: entitlements.NewGormStore(d.ORM), Tx: d.Tx, Audit: &auditLog{},
		Idempotency: idem, Clock: clk, Log: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	ents := entsMod.Service
	two := 2
	if _, err := ents.Grant(ctx, entitlements.GrantInput{OrgID: company, Feature: entitlements.FeatureTargetedPost, Quantity: &two,
		ValidFrom: clk.Now().Add(-time.Hour), ValidTo: clk.Now().Add(24 * time.Hour), Status: entitlements.StatusActive,
		SourceType: entitlements.SourceAdminGrant, GrantedBy: &admin}); err != nil {
		t.Fatal(err)
	}
	left := func() int {
		t.Helper()
		av, err := ents.Available(ctx, company, entitlements.FeatureTargetedPost)
		if err != nil {
			t.Fatal(err)
		}
		return av.Remaining
	}
	usages := func() int {
		t.Helper()
		var n int64
		if err := d.ORM.Raw(`SELECT count(*) FROM entitlement_usages WHERE org_id = ?`, company).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return int(n)
	}
	orgsDir := newFakeOrgs()
	orgsDir.orgs[company] = orgs.OrgSummary{ID: company, Kind: orgs.KindCompany, Name: "Şirket", Slug: "sirket", Status: orgs.StatusActive}
	actor := authz.Actor{Kind: authz.KindUser, UserID: author,
		Memberships: []authz.Membership{{OrgID: company, OrgKind: authz.OrgCompany, Role: authz.MemberRecruiter}}}
	audits := &failingAudit{fail: string(authz.FeedPostCreateTargeted)}
	store := feed.NewGormStore(d.ORM)
	mod, err := feed.New(feed.Deps{
		Store: store, Tx: d.Tx, Outbox: outbox.NewMem(clk), Queue: queue.NewMem(), Audit: audits, Orgs: orgsDir,
		Idempotency: idem, Clock: clk, Log: obs.Discard(), Viewers: &fakeViewers{keys: map[uuid.UUID][]string{}},
		Targeting: fixedTargeting{key: "uni:" + uuid.NewV7().String()}, Entitlements: ents,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := feed.TargetedInput{Post: textInput("Staj"), Definition: &outreach.Definition{Uni: []uuid.UUID{uuid.NewV7()}}}

	// The audit write fails after Consume and the INSERT: everything rolls back.
	if _, err := mod.Service.CreateTargetedPost(ctx, actor, company, in); err == nil {
		t.Fatal("CreateTargetedPost succeeded with a failing audit")
	}
	if left() != 2 || usages() != 0 {
		t.Fatalf("after rollback: remaining %d, usage rows %d", left(), usages())
	}
	if rows, _ := store.PostsOfAuthor(ctx, author); len(rows) != 0 {
		t.Fatalf("posts after rollback = %d", len(rows))
	}

	// It commits: one unit, one usage row, the post carries it.
	audits.fail = ""
	v, err := mod.Service.CreateTargetedPost(ctx, actor, company, in)
	if err != nil {
		t.Fatal(err)
	}
	if left() != 1 || usages() != 1 || v.Post.EntitlementUsageID == nil || v.Post.AudienceSizeEstimate == nil ||
		*v.Post.AudienceSizeEstimate != 40 || v.Remaining == nil || *v.Remaining != 1 {
		t.Fatalf("after commit: remaining %d, usages %d, post %+v", left(), usages(), v.Post)
	}

	// The delete fails after the release: the unit stays consumed.
	audits.fail = string(authz.FeedPostDelete)
	if err := mod.Service.DeletePost(ctx, actor, v.Post.ID); err == nil {
		t.Fatal("DeletePost succeeded with a failing audit")
	}
	if left() != 1 || usages() != 1 {
		t.Fatalf("after a failed delete: remaining %d, usage rows %d", left(), usages())
	}
	// The delete commits: the reversal row and the unit back.
	audits.fail = ""
	if err := mod.Service.DeletePost(ctx, actor, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	if left() != 2 || usages() != 2 {
		t.Fatalf("after delete: remaining %d, usage rows %d (consumption + reversal)", left(), usages())
	}
	if _, err := store.GetPost(ctx, v.Post.ID); !errors.Is(err, feed.ErrNotFound) {
		t.Fatalf("draft after delete = %v", err)
	}
}

// TestReport_DuplicateOpen_409_Gorm: the database guards the one open
// report per user and target (content_reports_open_uq): a second open row
// written past the store is a unique violation (409); the store's insert
// returns the open report instead (the service answers 200).
func TestReport_DuplicateOpen_409_Gorm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	ctx := t.Context()
	org, by, reporter := w.org(t, feed.AuthorClub), w.user(t), w.user(t)
	store := feed.NewGormStore(d.ORM)
	p, err := store.InsertPost(ctx, publishedAt(post(org, feed.AuthorClub, by), pub0))
	if err != nil {
		t.Fatal(err)
	}
	r, created, err := store.InsertReport(ctx, feed.Report{ID: uuid.NewV7(), ReporterUserID: reporter,
		TargetType: feed.TargetPost, TargetID: p.ID, Reason: feed.ReasonSpam})
	if err != nil || !created {
		t.Fatalf("InsertReport = %v, %v", created, err)
	}
	raw := d.ORM.WithContext(ctx).Exec(`INSERT INTO content_reports (reporter_user_id, target_type, target_id, reason)
		VALUES ( ?, 'post', ?, 'hate')`, reporter, p.ID).Error
	if !errors.Is(db.Translate(raw), problem.ErrConflict) {
		t.Fatalf("raw duplicate open report = %v (want a 409 unique violation)", raw)
	}
	again, created, err := store.InsertReport(ctx, feed.Report{ID: uuid.NewV7(), ReporterUserID: reporter,
		TargetType: feed.TargetPost, TargetID: p.ID, Reason: feed.ReasonHate})
	if err != nil || created || again.ID != r.ID {
		t.Fatalf("store duplicate = %+v, %v, %v", again, created, err)
	}
}

// TestModeratePost_AppendOnlyAction_Gorm (02 §12.10): moderation_actions
// takes no UPDATE nor DELETE (its trigger), not even by the table's
// owner; only report_id is cleared when the report is purged.
func TestModeratePost_AppendOnlyAction_Gorm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	ctx := t.Context()
	org, by, mod, reporter := w.org(t, feed.AuthorClub), w.user(t), w.user(t), w.user(t)
	store := feed.NewGormStore(d.ORM)
	p, err := store.InsertPost(ctx, publishedAt(post(org, feed.AuthorClub, by), pub0))
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := store.InsertReport(ctx, feed.Report{ID: uuid.NewV7(), ReporterUserID: reporter, TargetType: feed.TargetPost,
		TargetID: p.ID, Reason: feed.ReasonSpam})
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.InsertModerationAction(ctx, feed.ModerationAction{ID: uuid.NewV7(), ReportID: &r.ID,
		TargetType: feed.TargetPost, TargetID: p.ID, Action: feed.ModHide, ReasonCode: "spam", ActorUserID: mod})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ORM.WithContext(ctx).Exec(`UPDATE moderation_actions SET reason_code = 'other' WHERE id = ?`, m.ID).Error; err == nil {
		t.Fatal("UPDATE of a moderation action succeeded")
	}
	if err := d.ORM.WithContext(ctx).Exec(`DELETE FROM moderation_actions WHERE id = ?`, m.ID).Error; err == nil {
		t.Fatal("DELETE of a moderation action succeeded")
	}
	if _, err := store.DeleteReportsOf(ctx, reporter); err != nil {
		t.Fatalf("erasing the report: %v", err)
	}
	got, err := store.GetModerationAction(ctx, m.ID)
	if err != nil || got.ReportID != nil || got.ReasonCode != "spam" {
		t.Fatalf("action after the report's erasure = %+v, %v", got, err)
	}
}
