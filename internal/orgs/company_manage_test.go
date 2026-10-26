package orgs_test

import (
	"testing"

	"koro/internal/authz"
	"koro/internal/orgs"
)

// TestGetOrg_CompanyEditorsReadTheDomain (BE-127): getOrg of a company is
// the manage view for its owner, admins and platform admins (the edit
// form's e-mail domain), never for a recruiter, another user or an
// anonymous reader.
func TestGetOrg_CompanyEditorsReadTheDomain(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	in := companyInput("Alan Adı Yazılım")
	in.EmailDomain = "alanadi.example.test"
	founder := user()
	v, err := e.svc.CreateCompany(t.Context(), founder, in)
	if err != nil {
		t.Fatal(err)
	}
	id := v.Org.ID
	owner := as(founder, id, orgs.KindCompany, authz.MemberCompanyOwner)
	for name, c := range map[string]struct {
		a    authz.Actor
		want bool
	}{
		"owner":          {owner, true},
		"company_admin":  {e.member(t, id, authz.MemberCompanyAdmin), true},
		"recruiter":      {e.member(t, id, authz.MemberRecruiter), false},
		"platform_admin": {persona(t, "platform_admin"), true},
		"other user":     {user(), false},
		"anonymous":      {authz.Anonymous(), false},
	} {
		got, err := e.svc.GetOrg(t.Context(), c.a, id)
		if err != nil {
			if c.want {
				t.Errorf("%s: %v", name, err)
			}
			continue // pending_review: hidden from outsiders
		}
		if got.Manage != c.want {
			t.Errorf("%s: manage = %v, want %v", name, got.Manage, c.want)
		}
		d := orgs.ToPublicDTO(got)
		if (d.Company.EmailDomain == "alanadi.example.test") != c.want {
			t.Errorf("%s: email_domain = %q", name, d.Company.EmailDomain)
		}
	}
}
