package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"koro/internal/directory"
	"koro/internal/identity"
	"koro/internal/platform/civil"
)

// TestWire_StudentVerificationOutsideEduTR (BE-220, saha testi B-16): a
// university whose students mail outside .edu.tr (Sabancı: sabanciuniv.edu)
// is listed in the directory, and identity's startEduVerification accepts
// its addresses through hooks.domainResolver; an unlisted domain stays 422
// identity.edu_domain_unknown whatever its shape (BE-228: a subdomain of a
// listed domain too).
func TestWire_StudentVerificationOutsideEduTR(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	ctx := t.Context()
	res, err := e.c.directory.svc.ImportReference(ctx, directory.ReferenceData{
		Universities: []directory.RefUniversity{{YokCode: "T-220", Name: "Kanca Vakıf Üniversitesi", City: "İstanbul",
			Type: directory.UniversityFoundation}},
		Domains: []directory.RefDomain{{Domain: "kancauniv.edu", UniversityYokCode: "T-220", DomainKind: directory.DomainMixed}},
	})
	if err != nil || res.Domains.Created != 1 {
		t.Fatalf("import = %+v, %v", res, err)
	}
	uni, found, err := e.c.hooks.domainResolver(ctx, "kancauniv.edu")
	if err != nil || !found {
		t.Fatalf("domainResolver = %v, %v", found, err)
	}
	if u, err := e.c.directory.svc.GetUniversity(ctx, uni); err != nil || u.YokCode != "T-220" {
		t.Fatalf("resolved university = %+v, %v", u, err)
	}
	birth, err := civil.New(2003, time.April, 23)
	if err != nil {
		t.Fatal(err)
	}
	tok := e.session(t, e.user(t, &birth), identity.ClientMobile)
	do := e.http(t)
	if code, body := do(http.MethodPost, "/api/v1/me/edu-verifications", tok, `{"edu_email":"ada@kancauniv.edu"}`); code != http.StatusAccepted {
		t.Fatalf("listed domain outside .edu.tr = %d %s", code, body)
	}
	for _, addr := range []string{"ada@digeruniv.edu", "ada@ogr.kancauniv.edu"} {
		if code, body := do(http.MethodPost, "/api/v1/me/edu-verifications", tok, `{"edu_email":"`+addr+`"}`); code != http.StatusUnprocessableEntity ||
			!strings.Contains(body, "identity.edu_domain_unknown") || strings.Contains(body, "ada@") {
			t.Fatalf("unlisted domain %s = %d %s", addr, code, body)
		}
	}
}

// TestWire_StudentVerificationRefusesAlumniAndStaff (BE-228, T-443): with
// the real directory behind hooks.domainResolver, identity's
// startEduVerification accepts an address only under a listed student or
// mixed domain itself. A graduate's mailbox (alumni.<domain>, an alumni
// row, or merely a subdomain of the student domain), a staff domain and an
// unlisted student-looking subdomain are 422 identity.edu_domain_unknown
// (the document path), before any token is mailed.
func TestWire_StudentVerificationRefusesAlumniAndStaff(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	ctx := t.Context()
	res, err := e.c.directory.svc.ImportReference(ctx, directory.ReferenceData{
		Universities: []directory.RefUniversity{{YokCode: "T-228", Name: "Mezun Teknik Üniversitesi", City: "Ankara",
			Type: directory.UniversityState}},
		Domains: []directory.RefDomain{
			{Domain: "mezunteknik.edu.tr", UniversityYokCode: "T-228", DomainKind: directory.DomainMixed},
			{Domain: "alumni.mezunteknik.edu.tr", UniversityYokCode: "T-228", DomainKind: directory.DomainAlumni},
			{Domain: "personel.edu.tr", UniversityYokCode: "T-228", DomainKind: directory.DomainStaff},
			{Domain: "std.personel.edu.tr", UniversityYokCode: "T-228", DomainKind: directory.DomainStudent},
		},
	})
	if err != nil || res.Domains.Created != 4 {
		t.Fatalf("import = %+v, %v", res, err)
	}
	birth, err := civil.New(2002, time.March, 9)
	if err != nil {
		t.Fatal(err)
	}
	do := e.http(t)
	// One account per address: auth_email allows 3 starts an hour per user.
	start := func(addr string) (int, string) {
		tok := e.session(t, e.user(t, &birth), identity.ClientMobile)
		return do(http.MethodPost, "/api/v1/me/edu-verifications", tok, `{"edu_email":"`+addr+`"}`)
	}
	for _, addr := range []string{
		"ada.yildiz.2019@alumni.mezunteknik.edu.tr", // alumni row
		"ada@mezun.mezunteknik.edu.tr",              // unlisted subdomain of the mixed domain
		"ada@personel.edu.tr",                       // staff row
		"ada@ogr.std.personel.edu.tr",               // below the student subdomain
	} {
		if code, body := start(addr); code != http.StatusUnprocessableEntity ||
			!strings.Contains(body, "identity.edu_domain_unknown") || strings.Contains(body, addr) {
			t.Fatalf("%s = %d %s", addr, code, body)
		}
	}
	for _, addr := range []string{"ada@mezunteknik.edu.tr", "deniz@std.personel.edu.tr"} {
		if code, body := start(addr); code != http.StatusAccepted {
			t.Fatalf("%s = %d %s", addr, code, body)
		}
	}
}
