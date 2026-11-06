//go:build integration

package feed_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
	"koro/internal/platform/testutil"
)

// lateKeeper binds feed to orgs after both are built, as cmd/koro does.
type lateKeeper struct{ k orgs.LogoKeeper }

func (l *lateKeeper) KeepReplacedLogo(ctx context.Context, a authz.Actor, orgID, fileID uuid.UUID) (bool, error) {
	return l.k.KeepReplacedLogo(ctx, a, orgID, fileID)
}

// TestFeed_ReplacedLogo_KeptForReport_Gorm (BE-180, T-401) runs a report on
// an org and updateOrg over PostgreSQL with the real orgs and files services:
// the report keeps the profile; the logo replaced while it is open keeps its
// files row, attached to the org, retired and without its public copy, behind
// an org_dropped_logos row; the org cannot name it again; the moderator sees
// it; after the resolution a replaced logo is deleted (BE-155); the retention
// purge deletes the row and then the file; a file deleted another way (the
// scan's rejection) takes its row along (ON DELETE CASCADE).
func TestFeed_ReplacedLogo_KeptForReport_Gorm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	ctx := t.Context()
	clk := clock.NewFake(t0)
	objects := storage.NewMem(clk)
	public := files.PublicMedia{Bucket: "public-test", BaseURL: "https://media.koro.example/public"}
	idem := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()})
	fm, err := files.New(files.Deps{
		Store: files.NewGormStore(d.ORM), Objects: objects, Tx: d.Tx, Outbox: outbox.NewMem(clk),
		Queue: queue.NewMem(), Idempotency: idem, Clock: clk, Log: obs.Discard(), Public: public,
		Buckets: files.Buckets{Files: "koro-files-dev", Quarantine: "quarantine"}, Audit: &audittest.Recorder{},
	})
	if err != nil {
		t.Fatal(err)
	}
	keeper := &lateKeeper{}
	orgStore := orgs.NewGormStore(d.ORM)
	om, err := orgs.New(orgs.Deps{
		Store: orgStore, Tx: d.Tx, Outbox: outbox.NewMem(clk), Audit: &audittest.Recorder{}, Clock: clk, Log: obs.Discard(),
		Idempotency: idem, Files: fm.Service, LogoKeeper: keeper,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := feed.NewGormStore(d.ORM)
	mod, err := feed.New(feed.Deps{
		Store: store, Tx: d.Tx, Outbox: outbox.NewMem(clk), Queue: queue.NewMem(),
		Audit: &audittest.Recorder{}, Orgs: om.Service, Idempotency: idem, Clock: clk, Log: obs.Discard(),
		Viewers: &fakeViewers{keys: map[uuid.UUID][]string{}}, Files: fm.Service,
	})
	if err != nil {
		t.Fatal(err)
	}
	keeper.k = mod.Service

	id, err := om.Service.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Logosu Değişen Kulüp", Description: "İlk açıklama"})
	if err != nil {
		t.Fatal(err)
	}
	admin := authz.Actor{Kind: authz.KindUser, UserID: w.user(t),
		Memberships: []authz.Membership{{OrgID: id, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}}
	reporter := authz.Actor{Kind: authz.KindUser, UserID: w.user(t)}
	moderator := persona(t, "moderator")
	moderator.UserID = w.user(t) // assigned_to refers to users
	// logo inserts a cleared org_logo upload of the org with a public copy.
	logo := func() (uuid.UUID, string) {
		t.Helper()
		f := uuid.NewV7()
		key := "org_logo/" + f.String() + "-" + strings.Repeat("ab", 16) + ".png"
		if _, err := d.Pool.Exec(ctx, `INSERT INTO files (id, purpose, org_id, bucket, storage_key, declared_mime, size_bytes,
			sha256, status, scan_status, public_key)
			VALUES ($1, 'org_logo', $2, 'koro-files-dev', $3, 'image/png', 10, decode(repeat('ab', 32), 'hex'), 'active', 'clean', $4)`,
			f, id, "org_logo/2026/11/"+f.String(), key); err != nil {
			t.Fatal(err)
		}
		objects.Upload(public.Bucket, key, []byte("png"), "image/png")
		return f, key
	}
	o, err := orgStore.GetOrg(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	etag := httpx.ETag(o.UpdatedAt)
	update := func(p orgs.OrgPatch) error {
		t.Helper()
		v, err := om.Service.UpdateOrg(ctx, admin, id, etag, p)
		if err == nil {
			etag = v.ETag
		}
		return err
	}
	mustUpdate := func(p orgs.OrgPatch) {
		t.Helper()
		if err := update(p); err != nil {
			t.Fatal(err)
		}
	}
	publicGone := func(key string) bool {
		t.Helper()
		_, err := objects.Stat(ctx, public.Bucket, key)
		return err != nil
	}
	fileGone := func(f uuid.UUID) bool {
		t.Helper()
		_, err := fm.Service.Get(ctx, f)
		return errors.Is(err, files.ErrNotFound)
	}

	l1, k1 := logo()
	l2, k2 := logo()
	mustUpdate(orgs.OrgPatch{Logo: &l1})

	// The report keeps the profile with the logo it names.
	r, err := mod.Service.CreateReport(ctx, reporter, feed.ReportInput{TargetType: feed.TargetOrg, TargetID: id, Reason: feed.ReasonSpam})
	if err != nil {
		t.Fatal(err)
	}
	desc := "Yeni açıklama"
	mustUpdate(orgs.OrgPatch{Logo: &l2, Description: &desc})

	// The replaced logo stays for the moderators, out of public media.
	meta, err := fm.Service.Get(ctx, l1)
	if err != nil || !meta.Retired || meta.AttachedModule != "orgs" || meta.AttachedEntityID != id {
		t.Fatalf("kept logo = %+v, %v", meta, err)
	}
	if !publicGone(k1) || publicGone(k2) {
		t.Fatalf("public copies: kept gone %v, new gone %v", publicGone(k1), publicGone(k2))
	}
	if u, err := fm.Service.PublicURL(ctx, l1); err != nil || u != "" {
		t.Fatalf("PublicURL(kept) = %q, %v", u, err)
	}
	view, err := mod.Service.GetReport(ctx, moderator, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	dto := feed.ToReportDTO(view).Target.Org
	if s := dto.ReportedSnapshot; s == nil || s.Name != "Logosu Değişen Kulüp" || s.Description != "İlk açıklama" || s.LogoFileID != l1.String() || s.LogoURL == "" {
		t.Fatalf("snapshot = %+v", s)
	}
	if dto.Description != desc || dto.LogoFileID != l2.String() || len(dto.DroppedLogos) != 1 || dto.DroppedLogos[0].FileID != l1.String() || dto.DroppedLogos[0].URL == "" {
		t.Fatalf("org = %+v", dto)
	}
	// To the org the kept logo is gone: it cannot name it again.
	if err := update(orgs.OrgPatch{Logo: &l1}); err == nil {
		t.Fatal("the org named a kept logo again")
	}

	// A file deleted another way (the scan's rejection) takes its row along.
	l3, _ := logo()
	mustUpdate(orgs.OrgPatch{Logo: &l3}) // l2 is kept too
	if err := fm.Service.Delete(ctx, authz.SystemActor("feed.test"), l2); err != nil {
		t.Fatalf("Delete(kept logo): %v", err)
	}
	if got, err := store.DroppedLogosOf(ctx, id); err != nil || len(got) != 1 || got[0].FileID != l1 {
		t.Fatalf("kept logos after the deletion = %+v, %v", got, err)
	}

	// Resolved: the next replaced logo is deleted with its public copy.
	if _, err := mod.Service.ResolveReport(ctx, moderator, r.ID, feed.ResolveInput{Status: feed.ReportDismissed}); err != nil {
		t.Fatal(err)
	}
	mustUpdate(orgs.OrgPatch{ClearLogo: true})
	if !fileGone(l3) {
		t.Fatal("a logo replaced without an open report was kept")
	}

	// The retention purge takes the row, then the file.
	now := t0.AddDate(3, 0, 0)
	year := now.Sub(now.AddDate(0, -12, 0))
	res, err := mod.Sweeper.Sweep(ctx, privacy.RetentionPolicy{Code: feed.PolicyLogoDropped, Retention: year, LegalApproved: true}, now)
	if err != nil || res.Count != 1 {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	if got, err := store.DroppedLogosOf(ctx, id); err != nil || len(got) != 0 {
		t.Fatalf("kept logos after the purge = %+v, %v", got, err)
	}
	if !fileGone(l1) {
		t.Fatal("the purge left the kept logo's file")
	}
}
