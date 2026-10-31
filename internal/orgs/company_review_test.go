package orgs_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// Synthetic identifiers that pass the check digits (koro-backend is public:
// no real VKN or TCKN).
const (
	testVKN  = "1234567890"
	testTCKN = "10000000146"
)

// wantField asserts a 422 whose only field error is at loc and does not
// echo secret (05 §11.2: tax identifiers are never in a message).
func wantField(t *testing.T, err error, loc, secret string) {
	t.Helper()
	problemtest.WantProblem(t, err, 422, "validation_failed")
	var p *problem.Error
	if !errors.As(err, &p) || len(p.Fields()) != 1 || p.Fields()[0].Location != loc {
		t.Fatalf("field errors of %v, want one at %s", err, loc)
	}
	if secret != "" && strings.Contains(fmt.Sprint(err, p.Fields()), secret) {
		t.Fatalf("the error echoes the identifier: %v", p.Fields())
	}
}

// TestCreateCompany_TaxIdentity (BE-165, Q-45): a VKN is checked, stored
// and shown masked to the company and in full to a platform admin; a sole
// trader's TCKN is checked and never stored, returned or audited (06 H-60);
// an invalid or doubled identifier is a 422 that does not echo it; a
// missing identifier or tax office is 422 orgs.tax_id_required with coded
// field errors.
func TestCreateCompany_TaxIdentity(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	in := companyInput("Vergi Yazılım")
	in.TaxNo, in.TaxOffice = "123 456 78-90", "  Kadıköy "
	founder := user()
	v, err := e.svc.CreateCompany(t.Context(), founder, in)
	if err != nil {
		t.Fatal(err)
	}
	c := v.Company
	if c == nil || c.TaxIDKind == nil || *c.TaxIDKind != orgs.TaxIDVKN || c.TaxNo == nil || *c.TaxNo != testVKN ||
		c.TaxOffice == nil || *c.TaxOffice != "Kadıköy" || !c.TaxIDValid() {
		t.Fatalf("company = %+v", c)
	}
	d := orgs.ToOrgDTO(v).Company
	if d.TaxIDKind != "vkn" || d.TaxNo != "******7890" || !d.TaxNoMasked || !d.TaxIDValid || d.TaxOffice != "Kadıköy" {
		t.Fatalf("founder's OrgDTO company = %+v", d)
	}
	owner := as(founder, v.Org.ID, orgs.KindCompany, authz.MemberCompanyOwner)
	for name, c := range map[string]struct {
		a    authz.Actor
		want string
	}{
		"owner":          {owner, "******7890"},
		"company_admin":  {e.member(t, v.Org.ID, authz.MemberCompanyAdmin), "******7890"},
		"platform_admin": {pad(t), testVKN},
	} {
		got, err := e.svc.GetOrg(t.Context(), c.a, v.Org.ID)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		pub := orgs.ToPublicDTO(got).Company
		if pub.CompanyTaxDTO == nil || pub.TaxNo != c.want || !pub.TaxIDValid {
			t.Errorf("%s: getOrg company = %+v", name, pub.CompanyTaxDTO)
		}
	}
	rec, err := e.svc.GetOrg(t.Context(), e.member(t, v.Org.ID, authz.MemberRecruiter), v.Org.ID)
	if err != nil || orgs.ToPublicDTO(rec).Company.CompanyTaxDTO != nil {
		t.Fatalf("a recruiter reads the tax identity: %v", err)
	}
	if ev := e.audit.Of(authz.OrgsCompanyCreate); len(ev) != 1 || ev[0].Metadata["tax_id_valid"] != true ||
		strings.Contains(fmt.Sprint(ev[0].Metadata), testVKN) {
		t.Fatalf("audit = %+v", ev)
	}

	// A sole trader gives a TCKN: only its kind is kept.
	in = companyInput("Şahıs Atölyesi")
	in.TaxNo, in.TCKN = "", testTCKN
	v, err = e.svc.CreateCompany(t.Context(), user(), in)
	if err != nil {
		t.Fatal(err)
	}
	if c := v.Company; c.TaxIDKind == nil || *c.TaxIDKind != orgs.TaxIDTCKN || c.TaxNo != nil || !c.TaxIDValid() {
		t.Fatalf("TCKN company = %+v", c)
	}
	body, err := json.Marshal(orgs.ToOrgDTO(v))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), testTCKN) || !strings.Contains(string(body), `"tax_id_kind":"tckn"`) {
		t.Fatalf("OrgDTO of a TCKN company = %s", body)
	}
	for _, ev := range e.audit.Of(authz.OrgsCompanyCreate) {
		if strings.Contains(fmt.Sprint(ev.Metadata), testTCKN) {
			t.Fatal("the audit record holds the TCKN")
		}
	}

	// Refusals: a wrong check digit, a VKN as TCKN, both at once.
	for name, c := range map[string]struct {
		vkn, tckn, loc, secret string
	}{
		"VKN check digit":   {"1234567891", "", "body.tax_no", "1234567891"},
		"VKN letters":       {"12345678ab", "", "body.tax_no", ""},
		"TCKN check digits": {"", "10000000147", "body.tckn", "10000000147"},
		"TCKN of 10 digits": {"", testVKN, "body.tckn", testVKN},
		"both":              {testVKN, testTCKN, "body.tckn", testTCKN},
	} {
		in := companyInput("Hatalı " + name)
		in.TaxNo, in.TCKN = c.vkn, c.tckn
		_, err := e.svc.CreateCompany(t.Context(), user(), in)
		t.Run(name, func(t *testing.T) {
			t.Parallel() // checks the error computed above: no shared state
			wantField(t, err, c.loc, c.secret)
		})
	}
	in = companyInput("Uzun Daire")
	in.TaxOffice = strings.Repeat("ş", 101)
	_, err = e.svc.CreateCompany(t.Context(), user(), in)
	wantField(t, err, "body.tax_office", "")

	// Q-45: the identifier and the tax office are required.
	for name, c := range map[string]struct {
		vkn, office string
		want        []string
	}{
		"no identifier":      {"", "Kadıköy", []string{"body.tax_no"}},
		"blank identifier":   {"  ", "Kadıköy", []string{"body.tax_no"}},
		"no tax office":      {testVKN, " ", []string{"body.tax_office"}},
		"neither of the two": {"", "", []string{"body.tax_no", "body.tax_office"}},
	} {
		in := companyInput("Eksik " + name)
		in.TaxNo, in.TaxOffice = c.vkn, c.office
		_, err := e.svc.CreateCompany(t.Context(), user(), in)
		t.Run(name, func(t *testing.T) {
			t.Parallel() // checks the error computed above: no shared state
			wantTaxIDRequired(t, err, c.want...)
		})
	}
	if n := len(e.audit.Of(authz.OrgsCompanyCreate)); n != 2 {
		t.Fatalf("company records = %d, want the 2 complete applications", n)
	}
}

// wantTaxIDRequired asserts 422 orgs.tax_id_required with one coded field
// error at each of locs.
func wantTaxIDRequired(t *testing.T, err error, locs ...string) {
	t.Helper()
	problemtest.WantProblem(t, err, 422, "orgs.tax_id_required")
	var p *problem.Error
	if !errors.As(err, &p) || len(p.Fields()) != len(locs) {
		t.Fatalf("field errors of %v, want %v", err, locs)
	}
	for i, f := range p.Fields() {
		if f.Location != locs[i] || f.Code != "orgs.tax_id_required" {
			t.Errorf("field error %d = %+v, want %s with the code", i, f, locs[i])
		}
	}
}

// TestCreateCompany_DomainVerification (BE-165): the founder's verified
// account address at email_domain verifies it at once; another domain, a
// subdomain or a parent domain does not; a free e-mail provider is not a
// company domain (422).
func TestCreateCompany_DomainVerification(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	create := func(t *testing.T, name, address, domain string) (orgs.OrgView, error) {
		t.Helper()
		founder := user()
		if address != "" {
			e.users.add(founder.UserID, address, false)
		}
		in := companyInput(name)
		in.EmailDomain = domain
		return e.svc.CreateCompany(t.Context(), founder, in)
	}
	for name, c := range map[string]struct {
		address, domain string
		want            bool
	}{
		"address at the domain":  {"Ayse.Kaya@Acme-Yazilim.example.test", "acme-yazilim.example.test", true},
		"another domain":         {"ayse@baska.example.test", "acme-yazilim.example.test", false},
		"address at a subdomain": {"ayse@ik.acme-yazilim.example.test", "acme-yazilim.example.test", false},
		"address at the parent":  {"ayse@example.test", "acme-yazilim.example.test", false},
		"no account address":     {"", "acme-yazilim.example.test", false},
		"no domain":              {"ayse@acme-yazilim.example.test", "", false},
	} {
		v, err := create(t, "Alan "+name, c.address, c.domain)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := v.Company.DomainVerifiedAt != nil; got != c.want || v.Company.DomainVerified() != c.want {
			t.Errorf("%s: verified = %v, want %v", name, got, c.want)
		}
		if orgs.ToOrgDTO(v).Company.DomainVerified != c.want {
			t.Errorf("%s: domain_verified in the OrgDTO", name)
		}
		if ev := e.audit.Of(authz.OrgsCompanyCreate); ev[len(ev)-1].Metadata["domain_verified"] != c.want {
			t.Errorf("%s: audit domain_verified = %v", name, ev[len(ev)-1].Metadata["domain_verified"])
		}
	}
	for _, d := range []string{"gmail.com", "Hotmail.com.tr", "yandex.com"} {
		_, err := create(t, "Serbest "+d, "ayse@"+strings.ToLower(d), d)
		wantField(t, err, "body.email_domain", "")
	}
}

// TestUpdateOrg_CompanyDomainAndTax (BE-165, Q-45): a member editor whose
// address is at the new domain verifies it; a platform admin's own address
// never does; a change resets the verification; a TCKN replaces a VKN (the
// number goes); the identifier and the office are never cleared, while a
// company from before BE-165 without them keeps taking other changes.
func TestUpdateOrg_CompanyDomainAndTax(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	founder := user()
	e.users.add(founder.UserID, "kurucu@ilk.example.test", false)
	in := companyInput("Düzenlenen Şirket")
	in.EmailDomain, in.TaxNo = "ilk.example.test", testVKN
	v, err := e.svc.CreateCompany(t.Context(), founder, in)
	if err != nil || !v.Company.DomainVerified() {
		t.Fatalf("create = %+v, %v", v.Company, err)
	}
	owner := as(founder, v.Org.ID, orgs.KindCompany, authz.MemberCompanyOwner)
	update := func(t *testing.T, a authz.Actor, p orgs.OrgPatch) orgs.OrgView {
		t.Helper()
		cur, err := e.svc.GetOrg(t.Context(), a, v.Org.ID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := e.svc.UpdateOrg(t.Context(), a, v.Org.ID, cur.ETag, p)
		if err != nil {
			t.Fatalf("UpdateOrg: %v", err)
		}
		return got
	}
	str := func(s string) *string { return &s }

	// The same domain again keeps the verification.
	if got := update(t, owner, orgs.OrgPatch{EmailDomain: str("ilk.example.test")}); !got.Company.DomainVerified() {
		t.Fatal("re-sending the verified domain dropped its verification")
	}
	// A new domain the owner holds no address at: unverified.
	if got := update(t, owner, orgs.OrgPatch{EmailDomain: str("yeni.example.test")}); got.Company.DomainVerified() {
		t.Fatal("a new domain kept the old verification")
	}
	// A platform admin's own address never verifies a company's domain.
	admin := pad(t)
	e.users.add(admin.UserID, "yonetici@koro.example.test", false)
	if got := update(t, admin, orgs.OrgPatch{EmailDomain: str("koro.example.test")}); got.Company.DomainVerified() {
		t.Fatal("a platform admin's address verified the domain")
	} else if !got.FullTaxNo || orgs.ToOrgDTO(got).Company.TaxNo != testVKN {
		t.Fatal("the platform admin's updateOrg masks the VKN")
	}
	// A company admin whose address is at the domain verifies it.
	ca := e.member(t, v.Org.ID, authz.MemberCompanyAdmin)
	e.users.add(ca.UserID, "ik@koro.example.test", false)
	got := update(t, ca, orgs.OrgPatch{EmailDomain: str("koro.example.test")})
	if !got.Company.DomainVerified() || !got.Company.DomainVerifiedAt.Equal(e.clk.Now()) {
		t.Fatalf("the member's address did not verify the domain: %+v", got.Company)
	}
	if orgs.ToOrgDTO(got).Company.TaxNo != "******7890" {
		t.Fatal("the company admin reads the VKN unmasked")
	}
	if ev := e.audit.Of(authz.OrgsOrgUpdate); fmt.Sprint(ev[len(ev)-1].Metadata["fields"]) != "[email_domain domain_verified_at]" {
		t.Fatalf("audit fields = %v", ev[len(ev)-1].Metadata["fields"])
	}
	// A free provider is refused on update too.
	_, err = e.svc.UpdateOrg(t.Context(), owner, v.Org.ID, got.ETag, orgs.OrgPatch{EmailDomain: str("gmail.com")})
	wantField(t, err, "body.email_domain", "")

	// A TCKN replaces the VKN: the number is gone.
	got = update(t, owner, orgs.OrgPatch{TCKN: str(testTCKN), TaxOffice: str("Çankaya")})
	if c := got.Company; c.TaxIDKind == nil || *c.TaxIDKind != orgs.TaxIDTCKN || c.TaxNo != nil || *c.TaxOffice != "Çankaya" {
		t.Fatalf("after the TCKN: %+v", c)
	}
	_, err = e.svc.UpdateOrg(t.Context(), owner, v.Org.ID, got.ETag, orgs.OrgPatch{TaxNo: str("1234567891")})
	wantField(t, err, "body.tax_no", "1234567891")
	// Empty values never clear the identifier or the office (Q-45), and
	// the refusal comes before any write (the ETag stays).
	if got, err = e.svc.GetOrg(t.Context(), owner, v.Org.ID); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		p    orgs.OrgPatch
		want []string
	}{
		"empty VKN":           {orgs.OrgPatch{TaxNo: str("")}, []string{"body.tax_no"}},
		"empty TCKN":          {orgs.OrgPatch{TCKN: str(" ")}, []string{"body.tax_no"}},
		"empty office":        {orgs.OrgPatch{TaxOffice: str("")}, []string{"body.tax_office"}},
		"both emptied":        {orgs.OrgPatch{TaxNo: str(""), TCKN: str(""), TaxOffice: str("")}, []string{"body.tax_no", "body.tax_office"}},
		"office with a clear": {orgs.OrgPatch{TaxNo: str(""), TaxOffice: str("Ulus")}, []string{"body.tax_no"}},
	} {
		_, err := e.svc.UpdateOrg(t.Context(), owner, v.Org.ID, got.ETag, c.p)
		t.Run(name, func(t *testing.T) {
			t.Parallel() // checks the error computed above: no shared state
			wantTaxIDRequired(t, err, c.want...)
		})
	}
	if cur, err := e.svc.GetOrg(t.Context(), owner, v.Org.ID); err != nil || cur.Company.TaxIDKind == nil ||
		cur.Company.TaxOffice == nil || cur.ETag != got.ETag {
		t.Fatalf("a refused clear changed the company: %+v, %v", cur.Company, err)
	}
	// A company from before BE-165 (no tax identity) keeps taking other
	// changes and gives its identity later.
	legacy, err := e.svc.Create(t.Context(), orgs.CreateOrg{Kind: orgs.KindCompany, Name: "Eski Şirket"})
	if err != nil {
		t.Fatal(err)
	}
	lv, err := e.svc.GetOrg(t.Context(), admin, legacy)
	if err != nil {
		t.Fatal(err)
	}
	lv, err = e.svc.UpdateOrg(t.Context(), admin, legacy, lv.ETag, orgs.OrgPatch{SizeBand: str("1-10")})
	if err != nil || lv.Company.TaxIDValid() {
		t.Fatalf("legacy company update = %+v, %v", lv.Company, err)
	}
	lv, err = e.svc.UpdateOrg(t.Context(), admin, legacy, lv.ETag, orgs.OrgPatch{TaxNo: str(testVKN), TaxOffice: str("Ulus")})
	if err != nil || !lv.Company.TaxIDValid() || *lv.Company.TaxOffice != "Ulus" {
		t.Fatalf("legacy company gives its tax identity = %+v, %v", lv.Company, err)
	}
	if ev := e.audit.Of(authz.OrgsOrgUpdate); fmt.Sprint(ev[len(ev)-1].Metadata["fields"]) != "[tax_id tax_office]" {
		t.Fatalf("audit fields = %v", ev[len(ev)-1].Metadata["fields"])
	}
	// Tax fields on a club are refused like the other company fields.
	club, err := e.svc.Create(t.Context(), orgs.CreateOrg{Kind: orgs.KindClub, Name: "Vergi Kulübü"})
	if err != nil {
		t.Fatal(err)
	}
	cv, err := e.svc.GetOrg(t.Context(), admin, club)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.UpdateOrg(t.Context(), admin, club, cv.ETag, orgs.OrgPatch{TaxNo: str(testVKN)})
	problemtest.WantErr(t, err, orgs.ErrCompanyFieldOnly)
}

// TestAdminListOrgs_CompanyReviewFacts (BE-165): the admin list carries
// each company's tax_id_valid and domain_verified (no numbers; a company
// from before Q-45 shows false); other kinds carry none.
func TestAdminListOrgs_CompanyReviewFacts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	good := user()
	e.users.add(good.UserID, "kurucu@iyi.example.test", false)
	in := companyInput("İyi Başvuru")
	in.TaxNo, in.EmailDomain = testVKN, "iyi.example.test"
	if _, err := e.svc.CreateCompany(t.Context(), good, in); err != nil {
		t.Fatal(err)
	}
	// A company from before BE-165 (OrgAdmin.Create: an empty profile).
	if _, err := e.svc.Create(t.Context(), orgs.CreateOrg{Kind: orgs.KindCompany, Name: "Eksik Başvuru"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(t.Context(), orgs.CreateOrg{Kind: orgs.KindClub, Name: "Liste Kulübü"}); err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.AdminListOrgs(t.Context(), pad(t), orgs.AdminQuery{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]orgs.AdminOrgDTO{}
	for _, v := range p.Items {
		seen[v.Org.Name] = orgs.ToAdminDTO(v)
	}
	if c := seen["İyi Başvuru"].Company; c == nil || c.TaxIDKind != "vkn" || !c.TaxIDValid || !c.DomainVerified ||
		c.EmailDomain != "iyi.example.test" {
		t.Fatalf("good application = %+v", c)
	}
	if c := seen["Eksik Başvuru"].Company; c == nil || c.TaxIDValid || c.DomainVerified || c.TaxIDKind != "" || c.EmailDomain != "" {
		t.Fatalf("incomplete application = %+v", c)
	}
	if c := seen["Liste Kulübü"].Company; c != nil {
		t.Fatalf("a club carries company review facts: %+v", c)
	}
	body, err := json.Marshal(seen["İyi Başvuru"])
	if err != nil || strings.Contains(string(body), testVKN) {
		t.Fatalf("the admin list shows the VKN: %s, %v", body, err)
	}
}
