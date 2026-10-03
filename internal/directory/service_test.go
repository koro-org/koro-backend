package directory_test

import (
	"errors"
	"slices"
	"testing"
	"uuid"

	"koro/db/reference"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/identity"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
)

func TestUniversityByEmailDomain_SubdomainMatch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, err := e.svc.ImportReference(ctx, directory.ReferenceData{
		Universities: []directory.RefUniversity{
			{YokCode: "U-1", Name: "Birinci Üniversitesi", City: "İstanbul", Type: directory.UniversityState},
			{YokCode: "U-2", Name: "İkinci Üniversitesi", City: "Ankara", Type: directory.UniversityState},
			{YokCode: "U-3", Name: "Kapalı Üniversite", City: "İzmir", Type: directory.UniversityState},
		},
		Domains: []directory.RefDomain{
			{Domain: "birinci.edu.tr", UniversityYokCode: "U-1", DomainKind: directory.DomainMixed},
			// A more specific domain of another university wins (longest match).
			{Domain: "ogr.birinci.edu.tr", UniversityYokCode: "U-2", DomainKind: directory.DomainStudent},
			{Domain: "kapali.edu.tr", UniversityYokCode: "U-3", DomainKind: directory.DomainMixed},
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
	// Deactivate U-3: its domains resolve to nothing.
	if _, err := e.svc.AdminUpdateUniversity(ctx, pad(t), u3, directory.UniversityPatch{IsActive: new(false)}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		domain string
		want   uuid.UUID
		found  bool
	}{
		{"birinci.edu.tr", u1, true},
		{"BIRINCI.EDU.TR", u1, true}, // a machine identifier: ASCII lower case, never Turkish (İ/ı)
		{"  birinci.edu.tr.", u1, true},
		{"bil.birinci.edu.tr", u1, true},
		{"ogr.birinci.edu.tr", u2, true},
		{"a.b.ogr.birinci.edu.tr", u2, true},
		{"xbirinci.edu.tr", uuid.Nil(), false}, // a suffix must start at a label boundary
		{"birinci.edu.tr.example.test", uuid.Nil(), false},
		{"edu.tr", uuid.Nil(), false},
		{"kapali.edu.tr", uuid.Nil(), false},
		{"", uuid.Nil(), false},
	} {
		got, found, err := e.svc.UniversityByEmailDomain(ctx, tc.domain)
		if err != nil || found != tc.found || got != tc.want {
			t.Errorf("UniversityByEmailDomain(%q) = %v, %v, %v; want %v, %v", tc.domain, got, found, err, tc.want, tc.found)
		}
	}
	// An inactive domain resolves to nothing; the admin PUT reactivates it.
	if _, _, err := e.svc.AdminPutUniversityEmailDomain(ctx, pad(t), u1, "birinci.edu.tr",
		directory.DomainInput{DomainKind: directory.DomainMixed, IsActive: false}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := e.svc.UniversityByEmailDomain(ctx, "bil.birinci.edu.tr"); found {
		t.Fatal("an inactive domain still matches")
	}
	// A domain of another university is 409.
	_, _, err = e.svc.AdminPutUniversityEmailDomain(ctx, pad(t), u1, "ogr.birinci.edu.tr",
		directory.DomainInput{DomainKind: directory.DomainMixed, IsActive: true})
	wantCode(t, err, 409, "directory.domain_taken")
	// Only .edu.tr domains are registered.
	_, _, err = e.svc.AdminPutUniversityEmailDomain(ctx, pad(t), u1, "example.com",
		directory.DomainInput{DomainKind: directory.DomainMixed, IsActive: true})
	wantCode(t, err, 422, "validation_failed")
}

func TestSeedReference_Idempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	data, err := directory.ParseReference(reference.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Universities) < 3 || len(data.Domains) == 0 || len(data.Faculties) == 0 || len(data.Departments) == 0 ||
		len(data.Skills) == 0 {
		t.Fatalf("embedded reference data is incomplete: %+v", data)
	}
	first, err := e.svc.ImportReference(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if first.Universities.Created != len(data.Universities) || first.Domains.Created != len(data.Domains) ||
		first.Faculties.Created != len(data.Faculties) || first.Departments.Created != len(data.Departments) ||
		first.Skills.Created != len(data.Skills) {
		t.Fatalf("first run = %+v", first)
	}
	orgCount := func() int {
		page, err := e.orgs.AdminListOrgs(ctx, pad(t), orgs.AdminQuery{Kind: orgs.KindUniversity, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Items)
	}
	if n := orgCount(); n != len(data.Universities) {
		t.Fatalf("university orgs = %d, want %d", n, len(data.Universities))
	}
	second, err := e.svc.ImportReference(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	want := directory.ImportResult{
		Universities: directory.Counts{Unchanged: len(data.Universities)}, Domains: directory.Counts{Unchanged: len(data.Domains)},
		Faculties: directory.Counts{Unchanged: len(data.Faculties)}, Departments: directory.Counts{Unchanged: len(data.Departments)},
		Skills: directory.Counts{Unchanged: len(data.Skills)},
	}
	if second != want {
		t.Fatalf("second run = %+v, want everything unchanged", second)
	}
	if n := orgCount(); n != len(data.Universities) {
		t.Fatalf("a re-import opened new orgs: %d", n)
	}
	// The official list replaces a sample code: the same university (by
	// name) takes the new code, no new row or org (only the file changes).
	u0 := data.Universities[0]
	before, err := e.mem.UniversityByYokCode(ctx, u0.YokCode)
	if err != nil {
		t.Fatal(err)
	}
	renamed := data
	renamed.Universities = slices.Clone(data.Universities)
	renamed.Universities[0].YokCode = "1234"
	renamed.Domains = slices.Clone(data.Domains)
	for i := range renamed.Domains {
		if renamed.Domains[i].UniversityYokCode == u0.YokCode {
			renamed.Domains[i].UniversityYokCode = "1234"
		}
	}
	renamed.Faculties, renamed.Departments = nil, nil
	third, err := e.svc.ImportReference(ctx, renamed)
	if err != nil {
		t.Fatal(err)
	}
	if third.Universities.Updated != 1 || third.Universities.Created != 0 {
		t.Fatalf("code change = %+v", third.Universities)
	}
	after, err := e.mem.UniversityByYokCode(ctx, "1234")
	if err != nil || after.ID != before.ID || after.OrgID != before.OrgID {
		t.Fatalf("after the code change: %+v, %v (before %+v)", after, err, before)
	}
	// An admin's deactivation survives a re-import.
	if _, err := e.svc.AdminUpdateUniversity(ctx, pad(t), after.ID, directory.UniversityPatch{IsActive: new(false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ImportReference(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	if u, _ := e.mem.GetUniversity(ctx, after.ID); u.IsActive {
		t.Fatal("re-import reactivated a deactivated university")
	}
	// A row naming an unknown university fails the whole import.
	_, err = e.svc.ImportReference(ctx, directory.ReferenceData{
		Domains: []directory.RefDomain{{Domain: "yok.edu.tr", UniversityYokCode: "NO-SUCH", DomainKind: directory.DomainMixed}},
	})
	wantCode(t, err, 422, "validation_failed")
	// One system-actor audit record per run.
	if got := e.audit.of(authz.DirectoryUniversityManage); len(got) < 4 || got[0].SystemJob != "directory.seed_reference" {
		t.Fatalf("seed audit = %+v", got)
	}
}

func TestAdminCreateClub_CreatesOrgInSameTx(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := e.university(t, "U-1", "Ege Üniversitesi", "İzmir")
	v := e.club(t, uni, "Satranç Kulübü", directory.CategoryHobby)
	if len(e.orgs.creates) != 2 || len(e.store.clubInserts) != 1 {
		t.Fatalf("creates %v, club inserts %v", e.orgs.creates, e.store.clubInserts)
	}
	// The org and the club row share one transaction (01 §6.3, T-78).
	if tx := e.orgs.creates[1]; tx == 0 || tx != e.store.clubInserts[0] {
		t.Fatalf("org created in tx %d, club in tx %d", tx, e.store.clubInserts[0])
	}
	if v.Org.Org.Kind != orgs.KindClub || v.Org.Org.Status != orgs.StatusActive || v.Club.UniversityID != uni.ID ||
		v.Club.ClaimStatus != directory.ClaimUnclaimed || *v.Org.Org.City != "İzmir" {
		t.Fatalf("club view = %+v", v)
	}
	if ev := e.audit.of(authz.DirectoryClubCreate); len(ev) != 1 || *ev[0].OrgID != v.Club.OrgID {
		t.Fatalf("audit = %+v", ev)
	}
	// A failing club row fails the whole operation (the org goes with it in
	// PostgreSQL: TestAdminCreateClub_CreatesOrgInSameTx_Gorm).
	e.store.mu.Lock()
	e.store.failClub = problem.ErrConflict
	e.store.mu.Unlock()
	_, err := e.svc.AdminCreateClub(ctx, pad(t), directory.ClubInput{
		UniversityID: uni.ID, Name: "Başarısız Kulüp", Category: directory.CategoryArts, JoinPolicy: directory.JoinOpen,
	})
	wantErr(t, err, problem.ErrConflict)
	if a, b := e.orgs.creates[2], e.store.clubInserts[1]; a != b {
		t.Fatalf("failed attempt: org tx %d, club tx %d", a, b)
	}
	// Validation before any write: unknown university, sensitive category
	// without the flag, reason without the flag, non-admins.
	for _, tc := range []struct {
		in   directory.ClubInput
		code string
	}{
		{directory.ClubInput{UniversityID: uuid.NewV7(), Name: "X Kulübü", Category: directory.CategoryArts, JoinPolicy: directory.JoinOpen}, "validation_failed"},
		{directory.ClubInput{UniversityID: uni.ID, Name: "Y Kulübü", Category: directory.CategoryPolitical, JoinPolicy: directory.JoinOpen}, "directory.sensitivity_required"},
		{directory.ClubInput{UniversityID: uni.ID, Name: "Z Kulübü", Category: directory.CategoryArts, JoinPolicy: directory.JoinOpen, SensitiveReason: "health"}, "validation_failed"},
		{directory.ClubInput{UniversityID: uni.ID, Name: "W Kulübü", Category: directory.CategoryArts, JoinPolicy: directory.JoinOpen, IsSensitive: true}, "validation_failed"},
	} {
		_, err := e.svc.AdminCreateClub(ctx, pad(t), tc.in)
		wantCode(t, err, 422, tc.code)
	}
	_, err = e.svc.AdminCreateClub(ctx, clubAdmin(v.Club.OrgID), directory.ClubInput{UniversityID: uni.ID})
	wantErr(t, err, authz.ErrForbidden)
}

func TestSetSensitivity_EmitsEventInSameTx(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := e.university(t, "U-1", "Ege Üniversitesi", "İzmir")
	v := e.club(t, uni, "Düşünce Kulübü", directory.CategoryCulture)
	id := v.Club.OrgID
	proof := identity.ReauthProof{Password: goodPassword}
	set := directory.SensitivityInput{IsSensitive: true, SensitiveReason: "philosophical_belief"}

	// R: no proof, wrong proof → 403, nothing written.
	_, err := e.svc.AdminSetClubSensitivity(ctx, pad(t), id, set, identity.ReauthProof{})
	wantCode(t, err, 403, "identity.reauth_required")
	_, err = e.svc.AdminSetClubSensitivity(ctx, pad(t), id, set, identity.ReauthProof{Password: "yanlis-parola-1"})
	wantCode(t, err, 403, "identity.reauth_failed")
	// A failing audit write fails the transaction: no event is committed
	// (another club: the in-memory store has no rollback).
	scratch := e.club(t, uni, "Deneme Kulübü", directory.CategoryCulture)
	e.audit.setFail(errors.New("audit down"))
	if _, err := e.svc.AdminSetClubSensitivity(ctx, pad(t), scratch.Club.OrgID, set, proof); err == nil {
		t.Fatal("set sensitivity with a failing audit succeeded")
	}
	e.audit.setFail(nil)
	if len(e.out.Topics()) != 0 {
		t.Fatalf("events after failures: %v", e.out.Topics())
	}

	got, err := e.svc.AdminSetClubSensitivity(ctx, pad(t), id, set, proof)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Club.IsSensitive || got.Club.SensitivitySetAt == nil || *got.Club.SensitivitySetBy != pad(t).UserID ||
		got.MemberCount != nil {
		t.Fatalf("club after set = %+v", got.Club)
	}
	evs := e.out.Events()
	if len(evs) != 1 || evs[0].Topic != domainevent.TopicClubSensitivityChanged || evs[0].AggregateID != id {
		t.Fatalf("events = %+v", evs)
	}
	p, err := domainevent.Decode[domainevent.ClubSensitivityChanged](evs[0])
	if err != nil || p.ClubOrgID != id || !p.Sensitive {
		t.Fatalf("payload = %+v, %v", p, err)
	}
	if spec, _ := domainevent.Lookup(domainevent.TopicClubSensitivityChanged); spec.Mechanism != domainevent.Outbox {
		t.Fatal("club.sensitivity_changed must be an outbox (O) topic")
	}
	audits := e.audit.of(authz.DirectoryClubSetSensitivity)
	if len(audits) != 1 || audits[0].Metadata["reauth"] != "password" || audits[0].Metadata["sensitive"] != true {
		t.Fatalf("audit = %+v", audits)
	}
	// The same flag again: nothing changes, no event. Another reason: an
	// audit record, no event (the audience is unaffected).
	if _, err := e.svc.AdminSetClubSensitivity(ctx, pad(t), id, set, proof); err != nil {
		t.Fatal(err)
	}
	set.SensitiveReason = "other"
	if _, err := e.svc.AdminSetClubSensitivity(ctx, pad(t), id, set, proof); err != nil {
		t.Fatal(err)
	}
	if n := len(e.out.Events()); n != 1 {
		t.Fatalf("events after no-op and reason change = %d, want 1", n)
	}
	// Clearing the flag emits Sensitive=false.
	if _, err := e.svc.AdminSetClubSensitivity(ctx, pad(t), id, directory.SensitivityInput{}, proof); err != nil {
		t.Fatal(err)
	}
	evs = e.out.Events()
	p, _ = domainevent.Decode[domainevent.ClubSensitivityChanged](evs[len(evs)-1])
	if len(evs) != 2 || p.Sensitive {
		t.Fatalf("events after clearing = %+v", evs)
	}
	// Non-admins are refused before anything is read; an unknown club is 404.
	_, err = e.svc.AdminSetClubSensitivity(ctx, clubAdmin(id), id, set, proof)
	wantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.AdminSetClubSensitivity(ctx, pad(t), uuid.NewV7(), set, proof)
	wantErr(t, err, directory.ErrNotFound)
}

func TestSensitiveCategory_RequiresFlag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := e.university(t, "U-1", "Ege Üniversitesi", "İzmir")
	v := e.club(t, uni, "Gençlik Kulübü", directory.CategoryCulture)
	admin := clubAdmin(v.Club.OrgID)
	// 03 k7: the club admin cannot move the club to a sensitive category.
	for _, cat := range []directory.Category{directory.CategoryReligious, directory.CategoryPolitical, directory.CategoryIdentity} {
		_, err := e.svc.UpdateClub(ctx, admin, v.Club.OrgID, v.ETag, directory.ClubPatch{Category: new(cat)})
		wantCode(t, err, 422, "directory.sensitivity_required")
	}
	// Neither can the platform admin while the flag is off …
	_, err := e.svc.UpdateClub(ctx, pad(t), v.Club.OrgID, v.ETag, directory.ClubPatch{Category: new(directory.CategoryReligious)})
	wantCode(t, err, 422, "directory.sensitivity_required")
	// … but once flagged the category is allowed, and the flag of a
	// religious club cannot be cleared.
	proof := identity.ReauthProof{Password: goodPassword}
	flagged, err := e.svc.AdminSetClubSensitivity(ctx, pad(t), v.Club.OrgID,
		directory.SensitivityInput{IsSensitive: true, SensitiveReason: "religion"}, proof)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := e.svc.UpdateClub(ctx, admin, v.Club.OrgID, flagged.ETag, directory.ClubPatch{Category: new(directory.CategoryReligious)})
	if err != nil || rel.Club.Category != directory.CategoryReligious {
		t.Fatalf("religious category on a flagged club: %+v, %v", rel.Club, err)
	}
	_, err = e.svc.AdminSetClubSensitivity(ctx, pad(t), v.Club.OrgID, directory.SensitivityInput{}, proof)
	wantCode(t, err, 422, "directory.sensitivity_required")
	// The CHECK itself (store level): see the store contract
	// ("clubs: sensitive category requires the flag").
}

func TestGetClub_SensitiveHidesMemberCount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := e.university(t, "U-1", "Ege Üniversitesi", "İzmir")
	open := e.club(t, uni, "Fotoğraf Kulübü", directory.CategoryArts)
	small := e.club(t, uni, "Küçük Kulüp", directory.CategoryHobby)
	sensitive := e.club(t, uni, "İnanç Topluluğu", directory.CategoryReligious)
	e.members.set(open.Club.OrgID, 42)
	e.members.set(small.Club.OrgID, directory.MemberThreshold-1)
	e.members.set(sensitive.Club.OrgID, 120)
	for _, a := range []authz.Actor{authz.Anonymous(), persona(t, "company_admin"), pad(t)} {
		v, err := e.svc.GetClubView(ctx, a, open.Club.OrgID)
		if err != nil || v.MemberCount == nil || *v.MemberCount != 42 {
			t.Fatalf("open club count = %v, %v", v.MemberCount, err)
		}
		v, err = e.svc.GetClubView(ctx, a, small.Club.OrgID)
		if err != nil || v.MemberCount != nil {
			t.Fatalf("below k=10 count = %v, %v", v.MemberCount, err)
		}
		v, err = e.svc.GetClubView(ctx, a, sensitive.Club.OrgID)
		if err != nil || v.MemberCount != nil || !v.Club.IsSensitive {
			t.Fatalf("sensitive club count = %v, %v", v.MemberCount, err)
		}
	}
	// The list hides it the same way; the sensitive club is still listed.
	page, err := e.svc.ListClubs(ctx, authz.Anonymous(), directory.ClubQuery{})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("list = %+v, %v", page, err)
	}
	for _, c := range page.Items {
		switch c.Row.OrgID {
		case open.Club.OrgID:
			if c.MemberCount == nil || *c.MemberCount != 42 {
				t.Fatalf("listed open club count = %v", c.MemberCount)
			}
		default:
			if c.MemberCount != nil {
				t.Fatalf("listed %s count = %v", c.Row.Name, *c.MemberCount)
			}
		}
	}
	// SensitiveOrgIDs is the filter of company targeting (outreach, students).
	m, err := e.svc.SensitiveOrgIDs(ctx, []uuid.UUID{open.Club.OrgID, sensitive.Club.OrgID, uuid.NewV7()})
	if err != nil || len(m) != 1 || !m[sensitive.Club.OrgID] {
		t.Fatalf("SensitiveOrgIDs = %v, %v", m, err)
	}
	// Unknown club → 404.
	_, err = e.svc.GetClubView(ctx, authz.Anonymous(), uuid.NewV7())
	wantErr(t, err, directory.ErrNotFound)
}

func TestListClubs_TurkishSearch_IstanbulVariants(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := e.university(t, "U-1", "Ege Üniversitesi", "İzmir")
	for _, n := range []string{"İstanbul Gezginleri", "ISTANBUL Tiyatro Topluluğu", "Ankara Kültür Kulübü", "Işık Fotoğrafçılık", "Satranç Kulübü"} {
		e.club(t, uni, n, directory.CategoryCulture)
	}
	istanbul := []string{"ISTANBUL Tiyatro Topluluğu", "İstanbul Gezginleri"}
	for _, q := range []string{"istanbul", "İstanbul", "İSTANBUL", "ISTANBUL", "Istanbul", "ıstanbul", "istanbul "} {
		page, err := e.svc.ListClubs(ctx, authz.Anonymous(), directory.ClubQuery{Q: q})
		if err != nil {
			t.Fatal(err)
		}
		if got := names(page); !slices.Equal(got, istanbul) {
			t.Errorf("q=%q: %v, want %v", q, got, istanbul)
		}
	}
	// I/ı/İ/i and the other Turkish letters fold the same way.
	for q, want := range map[string][]string{
		"ışık":     {"Işık Fotoğrafçılık"},
		"ISIK":     {"Işık Fotoğrafçılık"},
		"satranc":  {"Satranç Kulübü"},
		"KÜLTÜR":   {"Ankara Kültür Kulübü"},
		"kultur":   {"Ankara Kültür Kulübü"},
		"tiyatro":  {"ISTANBUL Tiyatro Topluluğu"},
		"yokolsun": {},
	} {
		page, err := e.svc.ListClubs(ctx, authz.Anonymous(), directory.ClubQuery{Q: q})
		if err != nil {
			t.Fatal(err)
		}
		got := names(page)
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !slices.Equal(got, want) {
			t.Errorf("q=%q: %v, want %v", q, got, want)
		}
	}
	// Sorting by name follows Turkish order (I < İ, ı before i in tr).
	page, err := e.svc.ListClubs(ctx, authz.Anonymous(), directory.ClubQuery{Sort: directory.SortName, Limit: 2})
	if err != nil || len(page.Items) != 2 || page.Next == nil {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	rest, err := e.svc.ListClubs(ctx, authz.Anonymous(), directory.ClubQuery{Sort: directory.SortName, Limit: 10, After: page.Next})
	if err != nil || len(rest.Items) != 3 || rest.Next != nil {
		t.Fatalf("second page = %+v, %v", rest, err)
	}
	all := append(slices.Clone(page.Items), rest.Items...)
	var order []string
	for _, c := range all {
		order = append(order, c.Row.Name)
	}
	want := []string{"Ankara Kültür Kulübü", "ISTANBUL Tiyatro Topluluğu", "Işık Fotoğrafçılık", "İstanbul Gezginleri", "Satranç Kulübü"}
	if !slices.Equal(order, want) {
		t.Fatalf("Turkish name order = %v, want %v", order, want)
	}
	// Bad query: one character.
	_, err = e.svc.ListClubs(ctx, authz.Anonymous(), directory.ClubQuery{Q: "i"})
	wantCode(t, err, 422, "validation_failed")
}

func TestUpdateClub_StaleETag_412(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := e.university(t, "U-1", "Ege Üniversitesi", "İzmir")
	v := e.club(t, uni, "Dağcılık Kulübü", directory.CategorySports)
	admin := clubAdmin(v.Club.OrgID)
	open := directory.JoinOpen
	patch := directory.ClubPatch{JoinPolicy: &open}

	_, err := e.svc.UpdateClub(ctx, admin, v.Club.OrgID, "", patch)
	wantCode(t, err, 428, "precondition_required")
	_, err = e.svc.UpdateClub(ctx, admin, v.Club.OrgID, `"0"`, patch)
	wantCode(t, err, 412, "precondition_failed")
	got, err := e.svc.UpdateClub(ctx, admin, v.Club.OrgID, `"x", `+v.ETag, patch)
	if err != nil || got.Club.JoinPolicy != directory.JoinOpen || got.ETag == v.ETag {
		t.Fatalf("update = %+v, %v", got, err)
	}
	// The old ETag is stale now.
	_, err = e.svc.UpdateClub(ctx, admin, v.Club.OrgID, v.ETag, patch)
	wantCode(t, err, 412, "precondition_failed")
	if ev := e.audit.of(authz.DirectoryClubUpdate); len(ev) != 1 || !slices.Equal(ev[0].Metadata["fields"].([]string), []string{"join_policy"}) {
		t.Fatalf("audit = %+v", ev)
	}
	// Authorisation comes before the precondition: another club's admin
	// gets 403, not 428; an empty patch is 422.
	_, err = e.svc.UpdateClub(ctx, clubAdmin(uuid.NewV7()), v.Club.OrgID, "", patch)
	wantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.UpdateClub(ctx, admin, v.Club.OrgID, got.ETag, directory.ClubPatch{})
	wantCode(t, err, 422, "directory.empty_patch")
	// A suspended club takes no change from its admin (03 §2.3); the
	// platform admin may still act.
	o, err := e.orgsMem.GetOrg(ctx, v.Club.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	o.Status = orgs.StatusSuspended
	if _, err := e.orgsMem.UpdateOrg(ctx, o); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.UpdateClub(ctx, admin, v.Club.OrgID, got.ETag, patch)
	wantErr(t, err, orgs.ErrOrgSuspended)
	if _, err := e.svc.UpdateClub(ctx, pad(t), v.Club.OrgID, got.ETag, patch); err != nil {
		t.Fatalf("platform admin on a suspended club: %v", err)
	}
}

func TestSocialLinks_OnlyHTTPS(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := e.university(t, "U-1", "Ege Üniversitesi", "İzmir")
	v := e.club(t, uni, "Yazılım Kulübü", directory.CategoryTechnology)
	admin := clubAdmin(v.Club.OrgID)
	for name, links := range map[string]map[string]string{
		"http":        {"instagram": "http://instagram.com/yazilim"},
		"javascript":  {"instagram": "javascript:alert(1)"},
		"no host":     {"instagram": "https://"},
		"credentials": {"instagram": "https://user:pass@instagram.com/x"},
		"relative":    {"instagram": "//instagram.com/x"},
		"bad network": {"Insta Gram": "https://instagram.com/x"},
		"too many": {"a1": "https://a.example.test", "a2": "https://a.example.test", "a3": "https://a.example.test",
			"a4": "https://a.example.test", "a5": "https://a.example.test", "a6": "https://a.example.test",
			"a7": "https://a.example.test", "a8": "https://a.example.test", "a9": "https://a.example.test",
			"b1": "https://a.example.test", "b2": "https://a.example.test"},
	} {
		_, err := e.svc.UpdateClub(ctx, admin, v.Club.OrgID, v.ETag, directory.ClubPatch{SetSocialLinks: true, SocialLinks: links})
		var p *problem.Error
		if !errors.As(err, &p) || p.Status() != 422 {
			t.Errorf("%s: error = %v, want 422", name, err)
		}
	}
	got, err := e.svc.UpdateClub(ctx, admin, v.Club.OrgID, v.ETag, directory.ClubPatch{SetSocialLinks: true,
		SocialLinks: map[string]string{"instagram": " https://instagram.com/yazilim ", "linkedin": "https://www.linkedin.com/company/x"}})
	if err != nil || got.Club.SocialLinks["instagram"] != "https://instagram.com/yazilim" || len(got.Club.SocialLinks) != 2 {
		t.Fatalf("https links = %+v, %v", got.Club.SocialLinks, err)
	}
	// An empty map clears them.
	got, err = e.svc.UpdateClub(ctx, admin, v.Club.OrgID, got.ETag, directory.ClubPatch{SetSocialLinks: true, SocialLinks: map[string]string{}})
	if err != nil || len(got.Club.SocialLinks) != 0 {
		t.Fatalf("cleared links = %+v, %v", got.Club.SocialLinks, err)
	}
}

func TestRequestClubSensitivity_AuditsAndNotifiesAdmins(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := e.university(t, "U-1", "Ege Üniversitesi", "İzmir")
	v := e.club(t, uni, "Kültür Kulübü", directory.CategoryCulture)
	admin := clubAdmin(v.Club.OrgID)
	// Only the club's admin asks; platform admins set the flag directly.
	wantErr(t, e.svc.RequestClubSensitivity(ctx, pad(t), v.Club.OrgID, "religion", ""), authz.ErrForbidden)
	wantErr(t, e.svc.RequestClubSensitivity(ctx, clubAdmin(uuid.NewV7()), v.Club.OrgID, "religion", ""), authz.ErrForbidden)
	wantCode(t, e.svc.RequestClubSensitivity(ctx, admin, v.Club.OrgID, "nope", ""), 422, "validation_failed")
	if err := e.svc.RequestClubSensitivity(ctx, admin, v.Club.OrgID, "religion", "Kulüp bir inanç topluluğu"); err != nil {
		t.Fatal(err)
	}
	ev := e.audit.of(authz.DirectoryClubRequestSensitivity)
	if len(ev) != 1 || ev[0].Reason != "Kulüp bir inanç topluluğu" || ev[0].Metadata["sensitive_reason"] != "religion" {
		t.Fatalf("audit = %+v", ev)
	}
	tasks := e.queue.Tasks()
	if len(tasks) != 1 || tasks[0].Type != directory.SensitivityRequestTaskName {
		t.Fatalf("tasks = %+v", tasks)
	}
	// The worker sends one fixed in-app notice per platform admin (IDs only).
	task := e.mod.Tasks()[0]
	if err := task.Handle(ctx, tasks[0].Payload); err != nil {
		t.Fatal(err)
	}
	if len(e.sender.msgs) != len(e.admins) {
		t.Fatalf("messages = %d, want %d", len(e.sender.msgs), len(e.admins))
	}
	for i, m := range e.sender.msgs {
		if *m.UserID != e.admins[i] || m.Template != "directory.sensitivity_requested" || m.Data["club_org_id"] != v.Club.OrgID {
			t.Fatalf("message %d = %+v", i, m)
		}
	}
	// No flag yet: the club is not sensitive until a platform admin decides.
	c, _ := e.svc.GetClub(ctx, v.Club.OrgID)
	if c.IsSensitive {
		t.Fatal("a request set the flag")
	}
}

func TestCatalog_SkillsAndDepartments(t *testing.T) {
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
	py, err := e.mem.SkillBySlug(ctx, "python")
	if err != nil {
		t.Fatal(err)
	}
	ml, _ := e.mem.SkillBySlug(ctx, "machine-learning")
	if err := e.svc.SkillsExist(ctx, []uuid.UUID{py.ID, ml.ID, py.ID}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.svc.SkillsExist(ctx, []uuid.UUID{py.ID, uuid.NewV7()}), 422, "directory.unknown_skill")
	if _, err := e.svc.AdminUpdateSkill(ctx, pad(t), py.ID, directory.SkillPatch{IsActive: new(false)}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.svc.SkillsExist(ctx, []uuid.UUID{py.ID}), 422, "directory.unknown_skill")
	page, err := e.svc.ListSkills(ctx, authz.Anonymous(), directory.SkillQuery{Q: "makine", Limit: 100})
	if err != nil || len(page.Items) != 1 || page.Items[0].Name("en") != "Machine learning" {
		t.Fatalf("skills = %+v, %v", page, err)
	}
	unis, err := e.svc.ListUniversities(ctx, authz.Anonymous(), directory.UniversityQuery{Q: "İSTANBUL", Sort: directory.SortName})
	if err != nil || len(unis.Items) == 0 {
		t.Fatalf("universities = %+v, %v", unis, err)
	}
	var itu directory.University
	for _, u := range unis.Items {
		if u.City != "İstanbul" {
			t.Fatalf("search İSTANBUL returned %s (%s)", u.Name, u.City)
		}
		if u.Name == "İstanbul Teknik Üniversitesi" {
			itu = u
		}
	}
	view, err := e.svc.GetUniversityView(ctx, authz.Anonymous(), itu.ID)
	if err != nil || len(view.Faculties) == 0 {
		t.Fatalf("university view = %+v, %v", view, err)
	}
	deps, err := e.svc.ListDepartments(ctx, authz.Anonymous(), itu.ID, directory.DepartmentQuery{Q: "bilgisayar"})
	if err != nil || len(deps.Items) != 1 || deps.Items[0].Name != "Bilgisayar Mühendisliği" {
		t.Fatalf("departments = %+v, %v", deps, err)
	}
	d, err := e.svc.GetDepartment(ctx, deps.Items[0].ID)
	if err != nil || d.UniversityID != itu.ID {
		t.Fatalf("department = %+v, %v", d, err)
	}
	// A department under a faculty of another university is refused (H-38).
	other := e.university(t, "U-9", "Başka Üniversite", "Bursa")
	_, err = e.svc.AdminCreateDepartment(ctx, pad(t), other.ID, directory.DepartmentInput{
		FacultyID: &view.Faculties[0].ID, Name: "Yanlış Bölüm", DegreeLevel: directory.DegreeBachelor,
	})
	wantCode(t, err, 422, "validation_failed")
	// An inactive university is hidden from the public, not from the admin.
	if _, err := e.svc.AdminUpdateUniversity(ctx, pad(t), itu.ID, directory.UniversityPatch{IsActive: new(false)}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.GetUniversityView(ctx, authz.Anonymous(), itu.ID)
	wantErr(t, err, authz.ErrNotFound)
	if _, err := e.svc.GetUniversityView(ctx, pad(t), itu.ID); err != nil {
		t.Fatal(err)
	}
	// UniversityOrgs: the university's org and its clubs' orgs.
	c := e.club(t, other, "Bursa Kulübü", directory.CategoryHobby)
	ids, err := e.svc.UniversityOrgs(ctx, other.ID)
	if err != nil || !slices.Equal(ids, []uuid.UUID{other.OrgID, c.Club.OrgID}) {
		t.Fatalf("UniversityOrgs = %v, %v", ids, err)
	}
	if ids, err := e.svc.UniversityOrgs(ctx, uuid.NewV7()); err != nil || len(ids) != 0 {
		t.Fatalf("UniversityOrgs(unknown) = %v, %v", ids, err)
	}
}
