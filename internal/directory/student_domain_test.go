package directory_test

import (
	"errors"
	"strings"
	"testing"
	"uuid"

	"koro/db/reference"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// Student domains (BE-228, T-443): only an exact student or mixed row
// proves a student; subdomains of a listed domain (alumni.metu.edu.tr) and
// staff or alumni rows do not, while ownership (UniversityByEmailDomain,
// ingest) still covers them.

// TestStudentUniversityByEmailDomain_ExactStudentRowsOnly: the student
// resolution takes the address domain itself, an active student or mixed
// row of an active university; the ownership resolution of the same
// domains is unchanged (suffix, any kind).
func TestStudentUniversityByEmailDomain_ExactStudentRowsOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, err := e.svc.ImportReference(ctx, directory.ReferenceData{
		Universities: []directory.RefUniversity{
			{YokCode: "U-1", Name: "Birinci Üniversitesi", City: "Ankara", Type: directory.UniversityState},
			{YokCode: "U-2", Name: "İkinci Üniversitesi", City: "İstanbul", Type: directory.UniversityState},
			{YokCode: "U-3", Name: "Kapalı Üniversite", City: "İzmir", Type: directory.UniversityState},
		},
		Domains: []directory.RefDomain{
			{Domain: "birinci.edu.tr", UniversityYokCode: "U-1", DomainKind: directory.DomainMixed},
			{Domain: "alumni.birinci.edu.tr", UniversityYokCode: "U-1", DomainKind: directory.DomainAlumni},
			{Domain: "ikinci.edu.tr", UniversityYokCode: "U-2", DomainKind: directory.DomainStaff},
			{Domain: "std.ikinci.edu.tr", UniversityYokCode: "U-2", DomainKind: directory.DomainStudent},
			{Domain: "pasif.ikinci.edu.tr", UniversityYokCode: "U-2", DomainKind: directory.DomainStudent},
			{Domain: "kapali.edu.tr", UniversityYokCode: "U-3", DomainKind: directory.DomainStudent},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	uni := func(code string) uuid.UUID {
		u, err := e.mem.UniversityByYokCode(ctx, code)
		if err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	u1, u2, u3 := uni("U-1"), uni("U-2"), uni("U-3")
	if _, err := e.svc.AdminUpdateUniversity(ctx, pad(t), u3, directory.UniversityPatch{IsActive: new(false)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.AdminPutUniversityEmailDomain(ctx, pad(t), u2, "pasif.ikinci.edu.tr",
		directory.DomainInput{DomainKind: directory.DomainStudent, IsActive: false}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		domain         string
		student, owner uuid.UUID // uuid.Nil(): not found
	}{
		{"birinci.edu.tr", u1, u1},
		{"BIRINCI.EDU.TR.", u1, u1},                 // a machine identifier: ASCII lower case, trailing dot
		{"  birinci.edu.tr", u1, u1},                // surrounding space
		{"bil.birinci.edu.tr", uuid.Nil(), u1},      // a subdomain is not covered by its parent
		{"alumni.birinci.edu.tr", uuid.Nil(), u1},   // an alumni row proves nothing
		{"x.alumni.birinci.edu.tr", uuid.Nil(), u1}, // nor below it
		{"ikinci.edu.tr", uuid.Nil(), u2},           // a staff row proves nothing
		{"std.ikinci.edu.tr", u2, u2},               // a student subdomain listed on its own
		{"ogr.std.ikinci.edu.tr", uuid.Nil(), u2},
		{"pasif.ikinci.edu.tr", uuid.Nil(), u2}, // inactive row: the owner is the parent's
		{"kapali.edu.tr", uuid.Nil(), uuid.Nil()},
		{"edu.tr", uuid.Nil(), uuid.Nil()},
		{"xbirinci.edu.tr", uuid.Nil(), uuid.Nil()},
		{"", uuid.Nil(), uuid.Nil()},
	} {
		got, found, err := e.svc.StudentUniversityByEmailDomain(ctx, tc.domain)
		if err != nil || found != (tc.student != uuid.Nil()) || got != tc.student {
			t.Errorf("StudentUniversityByEmailDomain(%q) = %v, %v, %v; want %v", tc.domain, got, found, err, tc.student)
		}
		got, found, err = e.svc.UniversityByEmailDomain(ctx, tc.domain)
		if err != nil || found != (tc.owner != uuid.Nil()) || got != tc.owner {
			t.Errorf("UniversityByEmailDomain(%q) = %v, %v, %v; want %v", tc.domain, got, found, err, tc.owner)
		}
	}
}

// TestAdminPutUniversityEmailDomain_AlumniKind: an admin records an alumni
// domain with the alumni kind; a graduates' or retired staff's subdomain
// cannot be a student or mixed domain (422 on body.domain_kind, nothing
// written), and a kind outside the vocabulary names the four values.
func TestAdminPutUniversityEmailDomain_AlumniKind(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.university(t, "K-1", "Kanca Üniversitesi", "İstanbul")
	for _, d := range []string{"alumni.kanca.edu.tr", "alumni.kancauniv.edu"} {
		got, created, err := e.svc.AdminPutUniversityEmailDomain(ctx, pad(t), u.ID, d,
			directory.DomainInput{DomainKind: directory.DomainAlumni, IsActive: true})
		if err != nil || !created || got.DomainKind != directory.DomainAlumni {
			t.Fatalf("AdminPutUniversityEmailDomain(%s, alumni) = %+v, %v, %v", d, got, created, err)
		}
		if _, found, _ := e.svc.StudentUniversityByEmailDomain(ctx, d); found {
			t.Fatalf("%s: an alumni domain proves a student", d)
		}
	}
	if ev := e.audit.Of(authz.DirectoryUniversityManage); ev[len(ev)-1].Metadata["domain_kind"] != "alumni" {
		t.Fatalf("audit = %v", ev[len(ev)-1].Metadata)
	}
	// Retired staff under a staff kind is fine.
	if _, _, err := e.svc.AdminPutUniversityEmailDomain(ctx, pad(t), u.ID, "retired.kanca.edu.tr",
		directory.DomainInput{DomainKind: directory.DomainStaff, IsActive: true}); err != nil {
		t.Fatal(err)
	}
	before := len(e.audit.Of(authz.DirectoryUniversityManage))
	for _, tc := range []struct {
		domain string
		kind   directory.DomainKind
		msg    string
	}{
		{"mezun.kanca.edu.tr", directory.DomainStudent, "never proves a student"},
		{"ALUM.kanca.edu.tr", directory.DomainMixed, "never proves a student"},
		{"x.mezunlar.kanca.edu.tr", directory.DomainMixed, "never proves a student"},
		{"emekli.kanca.edu.tr", directory.DomainStudent, "never proves a student"},
		{"alumni.kanca.edu.tr", directory.DomainMixed, "never proves a student"}, // an existing row cannot be turned
		{"kanca.edu.tr", directory.DomainKind("faculty"), "student, staff, mixed, alumni"},
	} {
		_, _, err := e.svc.AdminPutUniversityEmailDomain(ctx, pad(t), u.ID, tc.domain,
			directory.DomainInput{DomainKind: tc.kind, IsActive: true})
		problemtest.WantProblem(t, err, 422, "validation_failed")
		pe, _ := errors.AsType[*problem.Error](err)
		if fe := pe.Fields(); len(fe) != 1 || fe[0].Location != "body.domain_kind" || !strings.Contains(fe[0].Message, tc.msg) {
			t.Fatalf("%s/%s: field errors %+v", tc.domain, tc.kind, fe)
		}
	}
	if n := len(e.audit.Of(authz.DirectoryUniversityManage)); n != before {
		t.Fatalf("%d audit records for refused kinds", n-before)
	}
	got, err := e.svc.AdminListUniversityEmailDomains(ctx, pad(t), u.ID)
	if err != nil || len(got) != 3 || got[0].Domain != "alumni.kanca.edu.tr" || got[0].DomainKind != directory.DomainAlumni {
		t.Fatalf("domains = %+v, %v", got, err)
	}
	// A university named like a label is not a subdomain of graduates.
	if _, _, err := e.svc.AdminPutUniversityEmailDomain(ctx, pad(t), u.ID, "mezun.edu.tr",
		directory.DomainInput{DomainKind: directory.DomainMixed, IsActive: true}); err != nil {
		t.Fatalf("mezun.edu.tr: %v", err)
	}
}

// TestImportReference_AlumniSubdomainNeedsAlumniKind: the reference list
// follows the same rule, located at the CSV row.
func TestImportReference_AlumniSubdomainNeedsAlumniKind(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := []directory.RefUniversity{{YokCode: "K-1", Name: "Kanca Üniversitesi", City: "İstanbul", Type: directory.UniversityState}}
	_, err := e.svc.ImportReference(ctx, directory.ReferenceData{Universities: uni, Domains: []directory.RefDomain{
		{Domain: "kanca.edu.tr", UniversityYokCode: "K-1", DomainKind: directory.DomainMixed},
		{Domain: "alumni.kanca.edu.tr", UniversityYokCode: "K-1", DomainKind: directory.DomainMixed},
	}})
	problemtest.WantProblem(t, err, 422, "validation_failed")
	pe, _ := errors.AsType[*problem.Error](err)
	if fe := pe.Fields(); len(fe) != 1 || fe[0].Location != directory.FileDomains+":2.domain_kind" {
		t.Fatalf("field errors %+v", fe)
	}
	res, err := e.svc.ImportReference(ctx, directory.ReferenceData{Universities: uni, Domains: []directory.RefDomain{
		{Domain: "kanca.edu.tr", UniversityYokCode: "K-1", DomainKind: directory.DomainMixed},
		{Domain: "alumni.kanca.edu.tr", UniversityYokCode: "K-1", DomainKind: directory.DomainAlumni},
	}})
	// The memory env has no real transaction: the first row of the refused
	// import may stay (the database rolls it back).
	if err != nil || res.Domains.Created+res.Domains.Unchanged != 2 {
		t.Fatalf("import = %+v, %v", res, err)
	}
	if _, found, _ := e.svc.StudentUniversityByEmailDomain(ctx, "alumni.kanca.edu.tr"); found {
		t.Fatal("an alumni reference row proves a student")
	}
}

// TestReferenceDomains_PilotStudentDomains pins the researched reference
// rows (db/reference/email_domains.csv, BE-228): Boğaziçi students mail
// under std.bogazici.edu.tr (bogazici.edu.tr and the retired boun.edu.tr
// are staff), ODTÜ and İTÜ students under the mixed main domain, and the
// alumni domains prove no student while still naming their university.
func TestReferenceDomains_PilotStudentDomains(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	data, err := directory.ParseReference(reference.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ImportReference(ctx, data); err != nil {
		t.Fatal(err)
	}
	uni := func(code string) uuid.UUID {
		u, err := e.mem.UniversityByYokCode(ctx, code)
		if err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	odtu, itu, boun := uni("ORNEK-ODTU"), uni("ORNEK-ITU"), uni("ORNEK-BOUN")
	for _, tc := range []struct {
		domain         string
		student, owner uuid.UUID
	}{
		{"metu.edu.tr", odtu, odtu},
		{"alumni.metu.edu.tr", uuid.Nil(), odtu},
		{"ceng.metu.edu.tr", uuid.Nil(), odtu},
		{"itu.edu.tr", itu, itu},
		{"std.bogazici.edu.tr", boun, boun},
		{"bogazici.edu.tr", uuid.Nil(), boun},
		{"boun.edu.tr", uuid.Nil(), boun},
		{"alumni.bogazici.edu.tr", uuid.Nil(), boun},
		{"pt.bogazici.edu.tr", uuid.Nil(), boun},
	} {
		got, found, err := e.svc.StudentUniversityByEmailDomain(ctx, tc.domain)
		if err != nil || found != (tc.student != uuid.Nil()) || got != tc.student {
			t.Errorf("StudentUniversityByEmailDomain(%q) = %v, %v, %v; want %v", tc.domain, got, found, err, tc.student)
		}
		if got, _, err := e.svc.UniversityByEmailDomain(ctx, tc.domain); err != nil || got != tc.owner {
			t.Errorf("UniversityByEmailDomain(%q) = %v, %v; want %v", tc.domain, got, err, tc.owner)
		}
	}
}
