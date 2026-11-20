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
// identity.edu_domain_unknown whatever its shape.
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
	uni, found, err := e.c.hooks.domainResolver(ctx, "ogr.kancauniv.edu")
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
	if code, body := do(http.MethodPost, "/api/v1/me/edu-verifications", tok, `{"edu_email":"ada@ogr.kancauniv.edu"}`); code != http.StatusAccepted {
		t.Fatalf("listed domain outside .edu.tr = %d %s", code, body)
	}
	if code, body := do(http.MethodPost, "/api/v1/me/edu-verifications", tok, `{"edu_email":"ada@digeruniv.edu"}`); code != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "identity.edu_domain_unknown") || strings.Contains(body, "ada@") {
		t.Fatalf("unlisted domain = %d %s", code, body)
	}
}
