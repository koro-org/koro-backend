package directory_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/orgs"
)

// logoFiles is the orgs files port of the club logo test: every logo is
// cleared and has a public copy.
type logoFiles struct{}

func (logoFiles) Get(context.Context, uuid.UUID) (files.FileMeta, error) {
	return files.FileMeta{}, files.ErrNotFound
}
func (logoFiles) Attach(context.Context, authz.Actor, uuid.UUID, files.Owner) error { return nil }
func (logoFiles) PresignGet(_ context.Context, id uuid.UUID, _ time.Duration) (files.Download, error) {
	return files.Download{URL: "https://s3.test/signed/" + id.String()}, nil
}
func (logoFiles) PublicURL(_ context.Context, id uuid.UUID) (string, error) {
	return "https://media.test/org_logo/" + id.String() + ".png", nil
}

// withOrgLogos wires orgs' logo files and its late club facts hook (the
// cmd/koro hooks.clubInfo: directory's GetClub).
func withOrgLogos(e *env, d *orgs.Deps) {
	d.Files = logoFiles{}
	d.Clubs = func(ctx context.Context, id uuid.UUID) (orgs.ClubFacts, bool, error) {
		c, err := e.svc.GetClub(ctx, id)
		switch {
		case errors.Is(err, directory.ErrNotFound):
			return orgs.ClubFacts{}, false, nil
		case err != nil:
			return orgs.ClubFacts{}, false, err
		}
		return orgs.ClubFacts{UniversityID: c.UniversityID, JoinPolicy: string(c.JoinPolicy), Sensitive: c.IsSensitive}, true, nil
	}
}

// TestListClubs_LogoPublicURL_BE120Rule (BE-126): a listClubs card carries
// the permanent logo URL of getClub's org.logo_public_url, under the same
// BE-120 rule: an active club that is not sensitive; a sensitive club, a
// suspended club and a club without a logo carry none.
func TestListClubs_LogoPublicURL_BE120Rule(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withOrgLogos)
	ctx := t.Context()
	uni := e.university(t, "U-1", "Ege Üniversitesi", "İzmir")
	open := e.club(t, uni, "Fotoğraf Kulübü", directory.CategoryArts)
	sensitive := e.club(t, uni, "İnanç Topluluğu", directory.CategoryReligious)
	suspended := e.club(t, uni, "Askıdaki Kulüp", directory.CategoryHobby)
	bare := e.club(t, uni, "Logosuz Kulüp", directory.CategoryHobby)
	logo := func(id uuid.UUID, st orgs.Status) {
		t.Helper()
		o, err := e.orgsMem.GetOrg(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		f := uuid.NewV7()
		o.LogoFileID, o.Status = &f, st
		if _, err := e.orgsMem.UpdateOrg(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	logo(open.Club.OrgID, orgs.StatusActive)
	logo(sensitive.Club.OrgID, orgs.StatusActive)
	logo(suspended.Club.OrgID, orgs.StatusSuspended)

	view, err := e.svc.GetClubView(ctx, authz.Anonymous(), open.Club.OrgID)
	if err != nil || view.Org.LogoPublicURL == "" {
		t.Fatalf("getClub org.logo_public_url = %q, %v", view.Org.LogoPublicURL, err)
	}
	page, err := e.svc.ListClubs(ctx, authz.Anonymous(), directory.ClubQuery{})
	if err != nil || len(page.Items) != 4 {
		t.Fatalf("list = %+v, %v", page, err)
	}
	want := map[uuid.UUID]string{open.Club.OrgID: view.Org.LogoPublicURL, sensitive.Club.OrgID: "",
		suspended.Club.OrgID: "", bare.Club.OrgID: ""}
	for _, c := range page.Items {
		if c.LogoPublicURL != want[c.Row.OrgID] {
			t.Errorf("%s card LogoPublicURL = %q, want %q", c.Row.Name, c.LogoPublicURL, want[c.Row.OrgID])
		}
	}
	// The JSON card (logo_public_url, omitted when empty).
	rec := call(t, newHTTP(t, e), "anonymous", http.MethodGet, "/api/v1/clubs", "", nil)
	var list directory.ClubCardList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); rec.Code != http.StatusOK || err != nil || len(list.Items) != 4 {
		t.Fatalf("GET /clubs = %d %s", rec.Code, rec.Body)
	}
	for _, c := range list.Items {
		id, _ := uuid.Parse(c.ClubID)
		if c.LogoPublicURL != want[id] {
			t.Errorf("GET /clubs %s logo_public_url = %q, want %q", c.Name, c.LogoPublicURL, want[id])
		}
	}
}
