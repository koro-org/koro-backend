package orgs_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/problem"
)

// logoUploads is the logo files port whose Get answers org_logo uploads of
// org and that records deletions.
type logoUploads struct {
	rejectFiles
	org     uuid.UUID
	retired uuid.UUID // a file kept for the moderators (BE-180)
}

func (f *logoUploads) Get(_ context.Context, id uuid.UUID) (files.FileMeta, error) {
	return files.FileMeta{ID: id, Purpose: files.PurposeOrgLogo, OrgID: &f.org, Status: files.StatusActive,
		ScanStatus: files.ScanClean, Retired: id == f.retired}, nil
}

// fakeKeeper is the moderators' claim on a replaced logo.
type fakeKeeper struct {
	keep  bool
	err   error
	asked [][2]uuid.UUID // org, file
}

func (k *fakeKeeper) KeepReplacedLogo(_ context.Context, _ authz.Actor, orgID, fileID uuid.UUID) (bool, error) {
	k.asked = append(k.asked, [2]uuid.UUID{orgID, fileID})
	return k.keep, k.err
}

// TestOrgs_ReplacedLogo_KeptForModerators (BE-180, T-401): updateOrg asks the
// keeper about the logo it replaces or removes, with the org of its locked
// row, after the row stops naming it. Kept, the file is left alone; not kept,
// it is deleted as ever; a failed keeper fails the update and deletes
// nothing. A patch that does not move the logo asks nothing, and the audit
// record says nothing of it: the org must not learn it was reported.
func TestOrgs_ReplacedLogo_KeptForModerators(t *testing.T) {
	t.Parallel()
	lf, keeper := &logoUploads{}, &fakeKeeper{}
	e := newEnv(t, func(d *orgs.Deps) { d.Files, d.LogoKeeper = lf, keeper })
	ctx := t.Context()
	v, owner := e.company(t, "Logolu Şirket")
	lf.org = v.Org.ID
	l1, l2, l3 := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	etag := v.ETag
	update := func(p orgs.OrgPatch) orgs.OrgView {
		t.Helper()
		got, err := e.svc.UpdateOrg(ctx, owner, v.Org.ID, etag, p)
		if err != nil {
			t.Fatal(err)
		}
		etag = got.ETag
		return got
	}
	desc := "Açıklama"
	update(orgs.OrgPatch{Logo: &l1}) // no logo before: nothing to ask about
	update(orgs.OrgPatch{Logo: &l1})
	update(orgs.OrgPatch{Description: &desc})
	if len(keeper.asked) != 0 {
		t.Fatalf("asked without a dropped logo: %v", keeper.asked)
	}

	// Kept: the row moves on, the file stays.
	keeper.keep = true
	if got := update(orgs.OrgPatch{Logo: &l2}); got.Org.LogoFileID == nil || *got.Org.LogoFileID != l2 || lf.wasDeleted(l1) {
		t.Fatalf("kept logo: %v, deleted %v", got.Org.LogoFileID, lf.deleted)
	}
	if want := [2]uuid.UUID{v.Org.ID, l1}; len(keeper.asked) != 1 || keeper.asked[0] != want {
		t.Fatalf("asked = %v, want %v", keeper.asked, want)
	}
	if meta := e.audit.Last().Metadata; meta["fields"] == nil || len(meta) != 1 {
		t.Fatalf("the update's audit says more than its fields: %v", meta)
	}

	// A failed keeper fails the update and deletes nothing.
	keeper.keep, keeper.err = false, errors.New("feed is down")
	if _, err := e.svc.UpdateOrg(ctx, owner, v.Org.ID, etag, orgs.OrgPatch{Logo: &l3}); !errors.Is(err, keeper.err) || lf.wasDeleted(l2) {
		t.Fatalf("failed keeper: %v, deleted %v", err, lf.deleted)
	}
	cur, err := e.store.GetOrg(ctx, v.Org.ID)
	if err != nil {
		t.Fatal(err)
	}
	keeper.err, etag = nil, httpx.ETag(cur.UpdatedAt)

	// Not kept (no open report): deleted as ever, removal included.
	update(orgs.OrgPatch{Logo: &l2})
	update(orgs.OrgPatch{Logo: &l3})
	if !lf.wasDeleted(l2) {
		t.Fatalf("a logo not kept stayed: deleted %v", lf.deleted)
	}
	if got := update(orgs.OrgPatch{ClearLogo: true}); got.Org.LogoFileID != nil || !lf.wasDeleted(l3) {
		t.Fatalf("removed logo: %v, deleted %v", got.Org.LogoFileID, lf.deleted)
	}
}

// TestOrgs_RetiredLogo_CannotComeBack (BE-180): to the org a logo kept for the
// moderators is gone, whichever way it asks: the answer of a deleted file.
func TestOrgs_RetiredLogo_CannotComeBack(t *testing.T) {
	t.Parallel()
	lf := &logoUploads{}
	e := newEnv(t, func(d *orgs.Deps) { d.Files = lf })
	v, owner := e.company(t, "Logolu Şirket")
	lf.org, lf.retired = v.Org.ID, uuid.NewV7()
	_, err := e.svc.UpdateOrg(t.Context(), owner, v.Org.ID, v.ETag, orgs.OrgPatch{Logo: &lf.retired})
	var pe *problem.Error
	if !errors.Is(err, problem.ErrValidation) || !errors.As(err, &pe) || len(pe.Fields()) != 1 || pe.Fields()[0].Location != "body.logo_file_id" {
		t.Fatalf("UpdateOrg(retired logo) = %v, want a 422 on body.logo_file_id", err)
	}
}

// TestOrgs_ReportedProfile (BE-180): the read of a report's snapshot.
func TestOrgs_ReportedProfile(t *testing.T) {
	t.Parallel()
	lf := &logoUploads{}
	e := newEnv(t, func(d *orgs.Deps) { d.Files = lf })
	ctx := t.Context()
	v, owner := e.company(t, "Profilli Şirket")
	lf.org = v.Org.ID
	logo, desc, site := uuid.NewV7(), "Açıklama", "https://a.koro.test"
	if _, err := e.svc.UpdateOrg(ctx, owner, v.Org.ID, v.ETag, orgs.OrgPatch{Logo: &logo, Description: &desc, WebsiteURL: &site}); err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.ReportedProfile(ctx, v.Org.ID)
	if err != nil || p.Kind != orgs.KindCompany || p.Name != "Profilli Şirket" || p.Description == nil || *p.Description != desc ||
		p.WebsiteURL == nil || *p.WebsiteURL != site || p.LogoFileID == nil || *p.LogoFileID != logo {
		t.Fatalf("ReportedProfile = %+v, %v", p, err)
	}
	if _, err := e.svc.ReportedProfile(ctx, uuid.NewV7()); !errors.Is(err, orgs.ErrNotFound) {
		t.Fatalf("ReportedProfile(unknown) = %v", err)
	}
}

// TestOrgs_ReplacedLogo_DeletesFile (BE-155, T-376): updateOrg deletes the
// logo it replaces or removes, in its transaction (with its public copy:
// files.Delete), after the row stops naming it; the same logo again and a
// patch without one delete nothing. org.updated revalidates the page.
func TestOrgs_ReplacedLogo_DeletesFile(t *testing.T) {
	t.Parallel()
	lf := &logoUploads{}
	e := newEnv(t, func(d *orgs.Deps) { d.Files = lf })
	ctx := t.Context()
	v, owner := e.company(t, "Logolu Şirket")
	lf.org = v.Org.ID
	l1, l2 := uuid.NewV7(), uuid.NewV7()
	etag := v.ETag
	update := func(p orgs.OrgPatch) orgs.OrgView {
		t.Helper()
		got, err := e.svc.UpdateOrg(ctx, owner, v.Org.ID, etag, p)
		if err != nil {
			t.Fatal(err)
		}
		etag = got.ETag
		return got
	}
	desc := "Açıklama"
	update(orgs.OrgPatch{Logo: &l1})
	update(orgs.OrgPatch{Logo: &l1})
	update(orgs.OrgPatch{Description: &desc})
	if len(lf.deleted) != 0 {
		t.Fatalf("deleted without a logo change: %v", lf.deleted)
	}
	topics := len(e.topics())
	if got := update(orgs.OrgPatch{Logo: &l2}); got.Org.LogoFileID == nil || *got.Org.LogoFileID != l2 || !lf.wasDeleted(l1) || lf.wasDeleted(l2) {
		t.Fatalf("replaced logo: %v, deleted %v", got.Org.LogoFileID, lf.deleted)
	}
	if got := e.topics()[topics:]; !slices.Contains(got, domainevent.TopicOrgUpdated) {
		t.Fatalf("a replaced logo wrote %v, want org.updated", got)
	}
	if got := update(orgs.OrgPatch{ClearLogo: true}); got.Org.LogoFileID != nil || !lf.wasDeleted(l2) || len(lf.deleted) != 2 {
		t.Fatalf("removed logo: %v, deleted %v", got.Org.LogoFileID, lf.deleted)
	}
}
