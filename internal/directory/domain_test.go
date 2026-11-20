package directory_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// University e-mail domains outside .edu.tr (BE-220, saha testi B-16).

// TestAdminPutUniversityEmailDomain_OutsideEduTR: a platform admin lists an
// official domain of a university that is not under .edu.tr (Sabancı mails
// under sabanciuniv.edu, Bilgi under bilgiedu.net); it resolves like a
// .edu.tr domain (subdomains, longest match) and the audit marks it.
func TestAdminPutUniversityEmailDomain_OutsideEduTR(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	e := newEnv(t, func(e *env, _ *orgs.Deps) { e.log = slog.New(slog.NewJSONHandler(logs, nil)) })
	ctx := t.Context()
	u := e.university(t, "K-1", "Kanca Üniversitesi", "İstanbul")
	for _, d := range []string{"sabanciuniv.edu", "KancaEdu.NET."} {
		got, created, err := e.svc.AdminPutUniversityEmailDomain(ctx, pad(t), u.ID, d,
			directory.DomainInput{DomainKind: directory.DomainMixed, IsActive: true})
		if err != nil || !created || got.UniversityID != u.ID || !got.IsActive {
			t.Fatalf("AdminPutUniversityEmailDomain(%s) = %+v, %v, %v", d, got, created, err)
		}
	}
	for _, d := range []string{"sabanciuniv.edu", "su.sabanciuniv.edu", "kancaedu.net"} {
		if id, found, err := e.svc.UniversityByEmailDomain(ctx, d); err != nil || !found || id != u.ID {
			t.Fatalf("UniversityByEmailDomain(%s) = %v, %v, %v", d, id, found, err)
		}
	}
	for _, d := range []string{"edu", "net", "sabanciuniv.edu.example.test", "xsabanciuniv.edu"} {
		if _, found, err := e.svc.UniversityByEmailDomain(ctx, d); err != nil || found {
			t.Fatalf("UniversityByEmailDomain(%s) matched (%v)", d, err)
		}
	}
	ev := e.audit.Of(authz.DirectoryUniversityManage)
	var marked []string
	for _, a := range ev {
		if a.Metadata["operation"] == "put_email_domain" && a.Metadata["edu_tr"] == false {
			marked = append(marked, a.Metadata["domain"].(string))
		}
	}
	if strings.Join(marked, ",") != "sabanciuniv.edu,kancaedu.net" {
		t.Fatalf("audit marks %v outside .edu.tr", marked)
	}
	// Each active addition outside .edu.tr leaves a WARN for review: the
	// domain (not personal data), never an address.
	if n := strings.Count(logs.String(), `"msg":"university e-mail domain outside .edu.tr"`); n != 2 ||
		!strings.Contains(logs.String(), `"domain":"sabanciuniv.edu"`) || !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Fatalf("logs %s", logs.String())
	}
	// A .edu.tr domain is marked edu_tr=true.
	if _, _, err := e.svc.AdminPutUniversityEmailDomain(ctx, pad(t), u.ID, "kanca.edu.tr",
		directory.DomainInput{DomainKind: directory.DomainStudent, IsActive: true}); err != nil {
		t.Fatal(err)
	}
	ev = e.audit.Of(authz.DirectoryUniversityManage)
	if last := ev[len(ev)-1].Metadata; last["domain"] != "kanca.edu.tr" || last["edu_tr"] != true {
		t.Fatalf("audit of a .edu.tr domain = %v", last)
	}
	if n := strings.Count(logs.String(), "outside .edu.tr"); n != 2 {
		t.Fatalf("%d WARN logs after a .edu.tr domain", n)
	}
}

// syncBuffer is a log sink safe for concurrent writers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestAdminPutUniversityEmailDomain_RefusesSuffixesAndSharedMail: no rule
// admits a domain by its shape alone; a public suffix (edu.tr, com.tr,
// github.io) or a shared mail service (gmail.com, a *.onmicrosoft.com
// tenant) is 422 on path.domain and nothing is written or audited.
func TestAdminPutUniversityEmailDomain_RefusesSuffixesAndSharedMail(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.university(t, "K-1", "Kanca Üniversitesi", "İstanbul")
	before := len(e.audit.Of(authz.DirectoryUniversityManage))
	for _, d := range []string{
		"edu.tr", "com.tr", "edu", "github.io", "gmail.com", "ogr.gmail.com", "outlook.com.tr", "hotmail.com", "yandex.com.tr",
		"kanca.onmicrosoft.com", "kanca uni.edu", "https://kanca.edu", "ada@kanca.edu", "kanca.edu.t", "kançauniv.edu", "",
		strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 55) + ".edu.tr",
	} {
		_, _, err := e.svc.AdminPutUniversityEmailDomain(ctx, pad(t), u.ID, d,
			directory.DomainInput{DomainKind: directory.DomainMixed, IsActive: true})
		problemtest.WantProblem(t, err, 422, "validation_failed")
		pe, _ := errors.AsType[*problem.Error](err)
		// The message never echoes the domain (edu.tr and edu are in its examples).
		echo := d != "" && d != "edu.tr" && d != "edu" && strings.Contains(fe0(pe).Message, d)
		if fe := pe.Fields(); len(fe) != 1 || fe[0].Location != "path.domain" || echo {
			t.Fatalf("%q: field errors %+v", d, fe)
		}
	}
	if got, err := e.svc.AdminListUniversityEmailDomains(ctx, pad(t), u.ID); err != nil || len(got) != 0 {
		t.Fatalf("domains written: %+v, %v", got, err)
	}
	if n := len(e.audit.Of(authz.DirectoryUniversityManage)); n != before {
		t.Fatalf("%d audit records for refused domains", n-before)
	}
}

// fe0 is the first field error of pe (a zero one when there is none).
func fe0(pe *problem.Error) problem.FieldError {
	if pe == nil || len(pe.Fields()) == 0 {
		return problem.FieldError{}
	}
	return pe.Fields()[0]
}

// TestImportReference_DomainOutsideEduTR: the reviewed reference list may
// carry an official domain outside .edu.tr; the same rule refuses a shared
// mail service there (located at the CSV row).
func TestImportReference_DomainOutsideEduTR(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := []directory.RefUniversity{{YokCode: "K-1", Name: "Kanca Üniversitesi", City: "İstanbul", Type: directory.UniversityFoundation}}
	res, err := e.svc.ImportReference(ctx, directory.ReferenceData{Universities: uni, Domains: []directory.RefDomain{
		{Domain: "kanca.edu.tr", UniversityYokCode: "K-1", DomainKind: directory.DomainMixed},
		{Domain: "sabanciuniv.edu", UniversityYokCode: "K-1", DomainKind: directory.DomainMixed},
	}})
	if err != nil || res.Domains.Created != 2 {
		t.Fatalf("import = %+v, %v", res, err)
	}
	u, err := e.mem.UniversityByYokCode(ctx, "K-1")
	if err != nil {
		t.Fatal(err)
	}
	if id, found, err := e.svc.UniversityByEmailDomain(ctx, "su.sabanciuniv.edu"); err != nil || !found || id != u.ID {
		t.Fatalf("UniversityByEmailDomain = %v, %v, %v", id, found, err)
	}
	for _, d := range []string{"gmail.com", "edu.tr"} {
		_, err = e.svc.ImportReference(ctx, directory.ReferenceData{Universities: uni, Domains: []directory.RefDomain{
			{Domain: "kanca.edu.tr", UniversityYokCode: "K-1", DomainKind: directory.DomainMixed},
			{Domain: d, UniversityYokCode: "K-1", DomainKind: directory.DomainMixed},
		}})
		problemtest.WantProblem(t, err, 422, "validation_failed")
		pe, _ := errors.AsType[*problem.Error](err)
		if fe := pe.Fields(); len(fe) != 1 || fe[0].Location != directory.FileDomains+":2.domain" {
			t.Fatalf("%s: field errors %+v", d, fe)
		}
	}
	if _, found, _ := e.svc.UniversityByEmailDomain(ctx, "gmail.com"); found {
		t.Fatal("a refused reference row was written")
	}
}
