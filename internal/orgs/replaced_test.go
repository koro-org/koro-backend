package orgs_test

import (
	"context"
	"slices"
	"testing"
	"uuid"

	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
)

// logoUploads is the logo files port whose Get answers org_logo uploads of
// org and that records deletions.
type logoUploads struct {
	rejectFiles
	org uuid.UUID
}

func (f *logoUploads) Get(_ context.Context, id uuid.UUID) (files.FileMeta, error) {
	return files.FileMeta{ID: id, Purpose: files.PurposeOrgLogo, OrgID: &f.org, Status: files.StatusActive,
		ScanStatus: files.ScanClean}, nil
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
