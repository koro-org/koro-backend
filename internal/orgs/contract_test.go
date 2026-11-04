package orgs_test

import (
	"net/http"
	"strings"
	"testing"

	"koro/internal/orgs"
	"koro/internal/platform/httpx/httpxtest"
)

// TestHTTP_GetOrg_ConformsToSchema (BE-168): getOrg and getOrgBySlug of an
// active company match the served OpenAPI schema for an anonymous reader
// (no tax identity: tax_id_valid and domain_verified are absent; the weekly
// Schemathesis run found them required) and for a platform admin (the
// editors' view with every tax field), as do a club's and a search page.
func TestHTTP_GetOrg_ConformsToSchema(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	in := companyInput("Sözleşme Yazılım")
	in.EmailDomain = "sozlesme.example.test"
	v, err := e.svc.CreateCompany(t.Context(), user(), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminVerifyOrg(t.Context(), pad(t), v.Org.ID, ""); err != nil {
		t.Fatal(err)
	}
	club, err := e.svc.Create(t.Context(), orgs.CreateOrg{Kind: orgs.KindClub, Name: "Sözleşme Kulübü"})
	if err != nil {
		t.Fatal(err)
	}
	app := newHTTP(t, e)
	for _, c := range []struct {
		name, persona, op, path string
		taxShown                bool
	}{
		{"anonymous company", "anonymous", "getOrg", "/api/v1/orgs/" + v.Org.ID.String(), false},
		{"anonymous company by slug", "anonymous", "getOrgBySlug", "/api/v1/orgs/by-slug/" + v.Org.Slug, false},
		{"platform admin company", "platform_admin", "getOrg", "/api/v1/orgs/" + v.Org.ID.String(), true},
		{"anonymous club", "anonymous", "getOrg", "/api/v1/orgs/" + club.String(), false},
		{"anonymous search", "anonymous", "searchOrgs", "/api/v1/orgs?q=sozlesme", false},
	} {
		rec := call(t, app, c.persona, http.MethodGet, c.path, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", c.name, rec.Code, rec.Body)
		}
		httpxtest.WantConforms(t, app.API, c.op, rec.Code, rec.Body.Bytes())
		if c.op != "searchOrgs" && strings.Contains(rec.Body.String(), `"tax_id_valid"`) != c.taxShown {
			t.Errorf("%s: tax facts shown = %v, want %v: %s", c.name, !c.taxShown, c.taxShown, rec.Body)
		}
	}
}
