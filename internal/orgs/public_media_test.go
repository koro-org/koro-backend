package orgs_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/orgs"
)

// logoFiles is the files port of the logo tests: every logo is cleared,
// signed and has a public copy.
type logoFiles struct{}

func (logoFiles) Get(context.Context, uuid.UUID) (files.FileMeta, error) {
	return files.FileMeta{}, files.ErrNotFound
}
func (logoFiles) Attach(context.Context, authz.Actor, uuid.UUID, files.Owner) error {
	return nil
}
func (logoFiles) PresignGet(_ context.Context, id uuid.UUID, _ time.Duration) (files.Download, error) {
	return files.Download{URL: "https://s3.test/signed/" + id.String()}, nil
}
func (logoFiles) PublicURL(_ context.Context, id uuid.UUID) (string, error) {
	return publicLogo(id), nil
}
func (logoFiles) Delete(context.Context, authz.Actor, uuid.UUID) error { return nil }

func publicLogo(id uuid.UUID) string { return "https://media.test/org_logo/" + id.String() + ".png" }

// withLogo gives the org a logo file (store write: the attach rules are
// updateOrg's and tested there) and sets its status.
func (e *env) withLogo(t *testing.T, id uuid.UUID, status orgs.Status) uuid.UUID {
	t.Helper()
	o, err := e.store.GetOrg(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	logo := uuid.NewV7()
	o.LogoFileID, o.Status = &logo, status
	if _, err := e.store.UpdateOrg(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	return logo
}

// TestOrgs_LogoPublicURL_OnlyPublicOrgs (BE-120): the permanent logo URL is
// shown for an active org that is not a sensitive club; a sensitive club,
// a club unknown to directory and a suspended org keep the signed URL only.
// PublicLogoURLs (BE-126) answers the same for a batch.
func TestOrgs_LogoPublicURL_OnlyPublicOrgs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *orgs.Deps) { d.Files = logoFiles{} })
	anon := authz.Anonymous()
	club := e.club(t, "Robotik Kulübü", "open", false)
	sensitive := e.club(t, "İnanç Topluluğu", "open", true)
	unknown, err := e.svc.Create(t.Context(), orgs.CreateOrg{Kind: orgs.KindClub, Name: "Dizinde Olmayan Kulüp"})
	if err != nil {
		t.Fatal(err)
	}
	company, _ := e.company(t, "Acme Yazılım")
	suspended, _ := e.company(t, "Askıdaki Şirket")
	logos := map[uuid.UUID]uuid.UUID{
		club: e.withLogo(t, club, orgs.StatusActive), sensitive: e.withLogo(t, sensitive, orgs.StatusActive),
		unknown: e.withLogo(t, unknown, orgs.StatusActive), company.Org.ID: e.withLogo(t, company.Org.ID, orgs.StatusActive),
		suspended.Org.ID: e.withLogo(t, suspended.Org.ID, orgs.StatusSuspended),
	}
	wantPublic := map[uuid.UUID]bool{club: true, company.Org.ID: true}
	for id, logo := range logos {
		v, err := e.svc.GetOrg(t.Context(), anon, id)
		if err != nil {
			t.Fatalf("GetOrg(%s): %v", v.Org.Name, err)
		}
		if v.LogoURL == "" {
			t.Errorf("%s: no signed logo_url", v.Org.Name)
		}
		if got, want := v.LogoPublicURL, wantPublic[id]; (got != "") != want || (want && got != publicLogo(logo)) {
			t.Errorf("%s (%s): logo_public_url = %q, want public %v", v.Org.Name, v.Org.Status, got, want)
		}
	}
	page, err := e.svc.SearchOrgs(t.Context(), anon, orgs.SearchQuery{Kind: orgs.KindClub})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range page.Items {
		if (v.LogoPublicURL != "") != wantPublic[v.Org.ID] {
			t.Errorf("searchOrgs card %s: logo_public_url = %q", v.Org.Name, v.LogoPublicURL)
		}
	}
	// BE-126: other modules' cards read the same rule in one call.
	ids := []uuid.UUID{uuid.NewV7()} // an unknown org is missing too
	for id := range logos {
		ids = append(ids, id)
	}
	got, err := e.svc.PublicLogoURLs(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(wantPublic) {
		t.Errorf("PublicLogoURLs = %v, want %d orgs", got, len(wantPublic))
	}
	for id := range wantPublic {
		if got[id] != publicLogo(logos[id]) {
			t.Errorf("PublicLogoURLs[%s] = %q", id, got[id])
		}
	}
	// Without the directory hook every club counts as sensitive.
	noDir := newEnv(t, func(d *orgs.Deps) { d.Files, d.Clubs = logoFiles{}, nil })
	c, err := noDir.svc.Create(t.Context(), orgs.CreateOrg{Kind: orgs.KindClub, Name: "Hooksuz Kulüp"})
	if err != nil {
		t.Fatal(err)
	}
	noDir.withLogo(t, c, orgs.StatusActive)
	if v, _ := noDir.svc.GetOrg(t.Context(), anon, c); v.LogoPublicURL != "" {
		t.Fatalf("club without directory facts: logo_public_url = %q", v.LogoPublicURL)
	}
}

// TestHTTP_ListCompanies_ActiveOnly (BE-120): GET /companies is public and
// lists active companies only (no pending_review, suspended, closed, no
// other kind), A-Z with sort=name, paged by cursor.
func TestHTTP_ListCompanies_ActiveOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *orgs.Deps) { d.Files = logoFiles{} })
	set := func(id uuid.UUID, st orgs.Status) {
		o, err := e.store.GetOrg(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		o.Status = st
		if st == orgs.StatusClosed {
			at := e.clk.Now()
			o.ClosedAt = &at
		}
		if _, err := e.store.UpdateOrg(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
	for name, st := range map[string]orgs.Status{
		"Zeta Lojistik": orgs.StatusActive, "Acme Yazılım": orgs.StatusActive, "Beklemede A.Ş.": orgs.StatusPendingReview,
		"Askıda Ltd.": orgs.StatusSuspended, "Kapalı Şirket": orgs.StatusClosed,
	} {
		v, _ := e.company(t, name)
		set(v.Org.ID, st)
	}
	e.withLogo(t, e.club(t, "Aktif Kulüp", "open", false), orgs.StatusActive)
	app := newHTTP(t, e)
	var names []string
	cursor := ""
	for range 3 {
		path := "/api/v1/companies?sort=name&limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := call(t, app, "anonymous", http.MethodGet, path, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("listCompanies = %d %s", rec.Code, rec.Body)
		}
		var page orgs.OrgCardList
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			if it.Kind != "company" {
				t.Fatalf("listCompanies returned a %s", it.Kind)
			}
			names = append(names, it.Name)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if len(names) != 2 || names[0] != "Acme Yazılım" || names[1] != "Zeta Lojistik" {
		t.Fatalf("listCompanies = %q, want the two active companies A-Z", names)
	}
	// The search rate class does not apply: 61 reads stay 200.
	for i := range 61 {
		if rec := call(t, app, "student", http.MethodGet, "/api/v1/companies", "", nil); rec.Code != http.StatusOK {
			t.Fatalf("read %d = %d", i+1, rec.Code)
		}
	}
}
