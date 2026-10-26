package parties_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/parties"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem/problemtest"
)

// TestLegalParty_TCKNNeverInDTO: an individual's national ID is stored
// envelope-encrypted with its blind index and never leaves the module: not
// in any response (not even masked: only whether one is on file, for
// finance and platform admins), not in the cross-module reads, not in the
// audit log (03 k21, 04 §2.13, 05 §11.2).
func TestLegalParty_TCKNNeverInDTO(t *testing.T) {
	t.Parallel()
	e := newEnv(t, naturalPersons)
	app := newHTTP(t, e, nil)
	body := map[string]any{
		"kind": "payee", "legal_type": "individual", "title": "Ayşe Yılmaz", "tckn": "100 000 001 46",
		"address": map[string]any{"line1": "Moda Cad. 5", "city": "İstanbul"}, "email": "ayse@ornek.test",
	}
	rec, out := call(t, app, "finance_admin", http.MethodPost, "/api/v1/admin/legal-parties", body, nil)
	wantStatus(t, rec, http.StatusCreated)
	noDigits(t, "admin create response", rec.Body.String(), tcknA)
	if out["tckn_on_file"] != true || out["status"] != "pending_verification" {
		t.Fatalf("admin create: %v", out)
	}
	id := uuid.MustParse(out["party_id"].(string))

	// Stored: sealed for its row, indexed, never in clear.
	row, err := e.store.GetParty(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(row.TCKNEnc) == 0 || bytes.Contains(row.TCKNEnc, []byte(tcknA)) || len(row.TCKNBidx) != 32 {
		t.Fatalf("stored national ID: enc %d bytes (clear: %v), bidx %d bytes", len(row.TCKNEnc),
			bytes.Contains(row.TCKNEnc, []byte(tcknA)), len(row.TCKNBidx))
	}
	_, idx := testKeys(t)
	if !bytes.Equal(row.TCKNBidx, idx.BlindIndex(parties.TCKNIndexPurpose, []byte(tcknA))) {
		t.Fatal("blind index is not the index of the normalised national ID")
	}

	// The admin list, the org surface (the club the person is paid for)
	// and a verification answer: no national ID.
	e.store.LinkPayee(uuid.NewV7(), org("club_a"), id, "active")
	rec, _ = call(t, app, "platform_admin", http.MethodPost, "/api/v1/admin/legal-parties/search", map[string]any{"kind": "payee"}, nil)
	wantStatus(t, rec, http.StatusOK)
	noDigits(t, "admin list", rec.Body.String(), tcknA)
	rec, out = call(t, app, "club_admin", http.MethodGet, orgPath("club_a", idPath(id)), nil, nil)
	wantStatus(t, rec, http.StatusOK)
	noDigits(t, "org get", rec.Body.String(), tcknA)
	if _, has := out["tckn_on_file"]; has {
		t.Fatalf("org roles do not learn whether a national ID is on file: %v", out)
	}
	rec, out = call(t, app, "finance_admin", http.MethodPost, "/api/v1/admin/legal-parties/"+id.String()+"/verification",
		map[string]any{"decision": "verified"}, nil)
	wantStatus(t, rec, http.StatusOK)
	noDigits(t, "verify response", rec.Body.String(), tcknA)
	if out["status"] != "verified" {
		t.Fatalf("an individual with a national ID and an address verifies: %v", out)
	}

	// Cross-module reads (orders, billing) never get the sealed value.
	got, err := e.svc.Get(t.Context(), id)
	if err != nil || got.TCKNEnc != nil || got.TCKNBidx != nil || !strings.Contains(dump(got), "Ayşe") {
		t.Fatalf("Parties.Get = %+v, %v", got, err)
	}
	noDigits(t, "audit log", dump(e.audit.All()), tcknA)

	// The DTO type has no national ID field at all.
	dt := reflect.TypeFor[parties.LegalPartyDTO]()
	for i := range dt.NumField() {
		f := dt.Field(i)
		if strings.Contains(strings.ToLower(f.Tag.Get("json")), "tckn") && f.Type.Kind() != reflect.Bool {
			t.Fatalf("LegalPartyDTO.%s carries a national ID", f.Name)
		}
	}
}

// TestLegalParty_VKNMaskedForOrgRoles: organization roles see the tax
// number and the MERSİS number masked to their last 4 digits; finance and
// platform admins see them in full on both surfaces.
func TestLegalParty_VKNMaskedForOrgRoles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e, nil)
	rec, out := call(t, app, "company_admin", http.MethodPost, orgPath("company_a"), map[string]any{
		"legal_type": "company", "title": "Örnek Yazılım A.Ş.", "tax_office": "Zincirlikuyu", "tax_no": vknA,
		"mersis_no": mersisA, "address": map[string]any{"line1": "Büyükdere Cad. 1", "city": "İstanbul"},
		"email": "fatura@ornek.test",
	}, nil)
	wantStatus(t, rec, http.StatusCreated)
	if out["tax_no"] != "******7890" || out["mersis_no"] != "************2345" || out["tax_ids_masked"] != true {
		t.Fatalf("create as company_admin: %v", out)
	}
	noDigits(t, "create response", rec.Body.String(), vknA, mersisA)
	id := out["party_id"].(string)
	for _, p := range []string{"company_member", "company_admin"} {
		rec, out = call(t, app, p, http.MethodGet, orgPath("company_a", "/"+id), nil, nil)
		wantStatus(t, rec, http.StatusOK)
		if out["tax_no"] != "******7890" || out["tax_ids_masked"] != true {
			t.Fatalf("%s: %v", p, out)
		}
		rec, _ = call(t, app, p, http.MethodGet, orgPath("company_a"), nil, nil)
		noDigits(t, p+" list", rec.Body.String(), vknA, mersisA)
	}
	f := e.foundation(t, vknB)
	rec, out = call(t, app, "club_admin", http.MethodGet, orgPath("club_a", idPath(f.Party.ID)), nil, nil)
	wantStatus(t, rec, http.StatusOK)
	if out["tax_no"] != "******3217" {
		t.Fatalf("club_admin sees the foundation masked: %v", out)
	}
	for _, p := range []string{"finance_admin", "platform_admin"} {
		rec, out = call(t, app, p, http.MethodGet, orgPath("company_a", "/"+id), nil, nil)
		wantStatus(t, rec, http.StatusOK)
		if out["tax_no"] != vknA || out["mersis_no"] != mersisA || out["tax_ids_masked"] != false {
			t.Fatalf("%s on the org surface: %v", p, out)
		}
		rec, _ = call(t, app, p, http.MethodPost, "/api/v1/admin/legal-parties/search", map[string]any{"q": vknA}, nil)
		wantStatus(t, rec, http.StatusOK)
		if !strings.Contains(rec.Body.String(), `"tax_no":"`+vknA+`"`) {
			t.Fatalf("%s admin search by VKN: %s", p, rec.Body.String())
		}
		// BE-128, BE-133: never in the URL; the GET list is gone.
		if rec, _ = call(t, app, p, http.MethodGet, "/api/v1/admin/legal-parties?q="+vknA, nil, nil); rec.Code != http.StatusNotFound &&
			rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s removed admin GET list = %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

// TestLegalParty_DuplicateTaxID_BlindIndex409: a VKN (per kind) and an
// individual's national ID (through its blind index, whatever the
// formatting) are unique among active parties; an inactive party frees
// them.
func TestLegalParty_DuplicateTaxID_BlindIndex409(t *testing.T) {
	t.Parallel()
	e := newEnv(t, naturalPersons)
	first := e.buyer(t, vknA)
	ocm := actor(t, "other_company_member") // company_b's admin
	_, err := e.svc.CreateBuyerParty(ctxOf(t, ocm), ocm, org("company_b"), buyerInput("123 456 7890"))
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.tax_id_taken")
	noDigits(t, "conflict error", err.Error(), vknA)
	// Another kind may carry the same VKN (a company's foundation, say).
	fin := actor(t, "finance_admin")
	if _, err := e.svc.AdminCreateParty(ctxOf(t, fin), fin, foundationInput(vknA)); err != nil {
		t.Fatalf("payee party with a buyer's VKN: %v", err)
	}
	// Changing another party onto a taken VKN is 409 too.
	second, err := e.svc.CreateBuyerParty(ctxOf(t, ocm), ocm, org("company_b"), buyerInput(vknC))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.UpdateOrgParty(ctxOf(t, ocm), ocm, org("company_b"), second.Party.ID, second.ETag, parties.PartyPatch{TaxNo: ptr(vknA)})
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.tax_id_taken")

	// National IDs: the blind index finds the duplicate.
	ind := func(tckn string) parties.PartyInput {
		return parties.PartyInput{Kind: parties.KindPayee, LegalType: parties.LegalIndividual, Title: "Ali Veli", TCKN: tckn}
	}
	if _, err := e.svc.AdminCreateParty(ctxOf(t, fin), fin, ind(tcknA)); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AdminCreateParty(ctxOf(t, fin), fin, ind("100-000-001-46"))
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.tax_id_taken")
	if _, err := e.svc.AdminCreateParty(ctxOf(t, fin), fin, ind(tcknB)); err != nil {
		t.Fatalf("another national ID: %v", err)
	}
	// An invalid national ID is 422 and never echoed.
	_, err = e.svc.AdminCreateParty(ctxOf(t, fin), fin, ind("10000000147"))
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	noDigits(t, "validation error", dump(err), "10000000147")

	// Retiring the first buyer party frees its VKN.
	cur, err := e.svc.GetOrgParty(ctxOf(t, fin), fin, org("company_a"), first.Party.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminUpdateParty(ctxOf(t, fin), fin, first.Party.ID, cur.ETag, parties.PartyPatch{Deactivate: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateBuyerParty(ctxOf(t, ocm), ocm, org("company_b"), buyerInput(vknA)); err != nil {
		t.Fatalf("VKN of an inactive party: %v", err)
	}
}

// TestUpdateVerifiedParty_ResetsToPendingVerification: a data change of a
// verified (or rejected) party sends it back to pending_verification
// (04 §2.13); If-Match is required; repeating the stored values changes
// nothing.
func TestUpdateVerifiedParty_ResetsToPendingVerification(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cma := actor(t, "company_admin")
	ctx := ctxOf(t, cma)
	created := e.buyer(t, vknA)
	if _, found, _ := e.svc.BuyerPartyOf(t.Context(), org("company_a")); found {
		t.Fatal("a pending party is not the company's buyer party yet")
	}
	verified := e.verify(t, created.Party.ID)
	if verified.Party.Status != parties.StatusVerified || verified.Party.VerifiedAt == nil || verified.Party.VerifiedBy == nil ||
		*verified.Party.VerifiedBy != authztest.ID("finance_admin") {
		t.Fatalf("verified = %+v", verified.Party)
	}
	if p, found, err := e.svc.BuyerPartyOf(t.Context(), org("company_a")); err != nil || !found || p.ID != created.Party.ID {
		t.Fatalf("BuyerPartyOf = %v %v %v", p.ID, found, err)
	}
	id := created.Party.ID
	patch := parties.PartyPatch{Title: ptr("Örnek Yazılım ve Danışmanlık A.Ş.")}
	_, err := e.svc.UpdateOrgParty(ctx, cma, org("company_a"), id, "", patch)
	problemtest.WantErr(t, err, parties.ErrPreconditionRequired)
	_, err = e.svc.UpdateOrgParty(ctx, cma, org("company_a"), id, created.ETag, patch) // the ETag before verification
	problemtest.WantErr(t, err, parties.ErrPreconditionFailed)

	e.clk.Advance(time.Minute)
	got, err := e.svc.UpdateOrgParty(ctx, cma, org("company_a"), id, verified.ETag, patch)
	if err != nil {
		t.Fatal(err)
	}
	if got.Party.Status != parties.StatusPendingVerification || got.Party.VerifiedAt != nil || got.Party.VerifiedBy != nil ||
		got.Party.Title != *patch.Title || got.ETag == verified.ETag {
		t.Fatalf("after the change: %+v (etag %s)", got.Party, got.ETag)
	}
	ev := e.audit.Of(authz.PartiesLegalPartyUpdate)
	if len(ev) != 1 || ev[0].Metadata["from"] != "verified" || ev[0].Metadata["to"] != "pending_verification" ||
		!slices.Equal(ev[0].Metadata["fields"].([]string), []string{"title"}) {
		t.Fatalf("update audit = %+v", ev)
	}
	if _, found, _ := e.svc.BuyerPartyOf(t.Context(), org("company_a")); found {
		t.Fatal("a changed party is not the buyer party until verified again")
	}

	// The same values again: nothing written, no new version, no audit.
	again := e.verify(t, id)
	same, err := e.svc.UpdateOrgParty(ctx, cma, org("company_a"), id, again.ETag, patch)
	if err != nil || same.Party.Status != parties.StatusVerified || same.ETag != again.ETag {
		t.Fatalf("no-op update: %+v %v", same.Party, err)
	}
	if n := len(e.audit.Of(authz.PartiesLegalPartyUpdate)); n != 1 {
		t.Fatalf("a no-op update is audited (%d events)", n)
	}
	_, err = e.svc.UpdateOrgParty(ctx, cma, org("company_a"), id, again.ETag, parties.PartyPatch{})
	problemtest.WantErr(t, err, parties.ErrEmptyPatch)

	// A rejected party goes back to the queue when it is corrected.
	fin := actor(t, "finance_admin")
	other, err := e.svc.CreateBuyerParty(ctx, cma, org("company_a"), buyerInput(vknC))
	if err != nil {
		t.Fatal(err)
	}
	rej, err := e.svc.AdminVerifyParty(ctxOf(t, fin), fin, other.Party.ID, parties.Decision{Decision: parties.StatusRejected, Note: "Vergi levhası okunmuyor"})
	if err != nil || rej.Party.Status != parties.StatusRejected {
		t.Fatalf("reject: %+v %v", rej.Party, err)
	}
	_, err = e.svc.AdminVerifyParty(ctxOf(t, fin), fin, other.Party.ID, parties.Decision{Decision: parties.StatusRejected})
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")
	fixed, err := e.svc.UpdateOrgParty(ctx, cma, org("company_a"), other.Party.ID, rej.ETag, parties.PartyPatch{TaxOffice: ptr("Mecidiyeköy")})
	if err != nil || fixed.Party.Status != parties.StatusPendingVerification {
		t.Fatalf("corrected rejected party: %+v %v", fixed.Party, err)
	}
	// Admin fields are refused on the org route; an inactive party is frozen.
	_, err = e.svc.UpdateOrgParty(ctx, cma, org("company_a"), other.Party.ID, fixed.ETag, parties.PartyPatch{Deactivate: true})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	off, err := e.svc.AdminUpdateParty(ctxOf(t, fin), fin, other.Party.ID, fixed.ETag, parties.PartyPatch{Deactivate: true})
	if err != nil || off.Party.Status != parties.StatusInactive {
		t.Fatalf("deactivate: %+v %v", off.Party, err)
	}
	_, err = e.svc.UpdateOrgParty(ctx, cma, org("company_a"), other.Party.ID, off.ETag, patch)
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")
}

// TestCreateBuyerParty_OnlyBuyerCompanyKind: the org route creates only
// buyer_company parties, of the company of the path (06 H-38: org_id from
// the path, never the body), with a VKN; payee and platform parties are
// finance and platform admins' (k22).
func TestCreateBuyerParty_OnlyBuyerCompanyKind(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cma := actor(t, "company_admin")
	in := buyerInput(vknA)
	in.Kind, in.OrgID = parties.KindPayee, ptr(org("company_b")) // ignored on the org route
	v, err := e.svc.CreateBuyerParty(ctxOf(t, cma), cma, org("company_a"), in)
	if err != nil {
		t.Fatal(err)
	}
	if v.Party.Kind != parties.KindBuyerCompany || v.Party.OrgID == nil || *v.Party.OrgID != org("company_a") ||
		v.Party.Status != parties.StatusPendingVerification || v.Party.CreatedBy == nil || *v.Party.CreatedBy != cma.UserID {
		t.Fatalf("created = %+v", v.Party)
	}
	if ev := e.audit.Of(authz.PartiesLegalPartyCreate); len(ev) != 1 || ev[0].OrgID == nil || *ev[0].OrgID != org("company_a") {
		t.Fatalf("create audit = %+v", ev)
	}

	// Only companies have buyer parties: a club admin has no role (403),
	// staff get 422.
	for _, tc := range []struct {
		persona string
		status  int
		code    string
	}{{"club_admin", 403, "forbidden"}, {"club_member", 403, "forbidden"}, {"platform_admin", 422, "parties.org_not_company"}} {
		a := actor(t, tc.persona)
		_, err := e.svc.CreateBuyerParty(ctxOf(t, a), a, org("club_a"), buyerInput(vknC))
		problemtest.WantProblem(t, err, tc.status, tc.code)
	}
	// A recruiter or another company's admin cannot create; an outsider does not see the surface.
	for p, status := range map[string]int{"company_member": 403, "other_company_member": 404, "student": 404} {
		a := actor(t, p)
		_, err := e.svc.CreateBuyerParty(ctxOf(t, a), a, org("company_a"), buyerInput(vknC))
		if s, _ := problemtest.StatusCode(err); s != status {
			t.Fatalf("%s: %v, want %d", p, err, status)
		}
	}
	// A buyer buys under a VKN: an individual is not a buyer type; VKN, tax office, address and e-mail are required.
	bad := buyerInput(vknC)
	bad.LegalType = parties.LegalIndividual
	_, err = e.svc.CreateBuyerParty(ctxOf(t, cma), cma, org("company_a"), bad)
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	for _, f := range []func(*parties.PartyInput){
		func(p *parties.PartyInput) { p.TaxNo = "" },
		func(p *parties.PartyInput) { p.TaxNo = "1234567891" }, // wrong check digit
		func(p *parties.PartyInput) { p.TaxOffice = "" },
		func(p *parties.PartyInput) { p.Address = nil },
		func(p *parties.PartyInput) { p.Email = "" },
		func(p *parties.PartyInput) { p.MersisNo = "12345" },
		func(p *parties.PartyInput) { p.Phone = "12" },
	} {
		in := buyerInput(vknC)
		f(&in)
		_, err := e.svc.CreateBuyerParty(ctxOf(t, cma), cma, org("company_a"), in)
		problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	}
	// The admin route takes payee and platform parties only.
	fin := actor(t, "finance_admin")
	adm := buyerInput(vknC)
	adm.Kind, adm.OrgID = parties.KindBuyerCompany, ptr(org("company_a"))
	_, err = e.svc.AdminCreateParty(ctxOf(t, fin), fin, adm)
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	// A payee may name the university it is, never a company or a club.
	uni := foundationInput(vknC)
	uni.LegalType, uni.OrgID = parties.LegalUniversity, ptr(org("uni_a"))
	if u, err := e.svc.AdminCreateParty(ctxOf(t, fin), fin, uni); err != nil || u.Party.OrgID == nil || *u.Party.OrgID != org("uni_a") {
		t.Fatalf("university party: %+v %v", u.Party, err)
	}
	uni.OrgID, uni.TaxNo = ptr(org("club_a")), vknD
	_, err = e.svc.AdminCreateParty(ctxOf(t, fin), fin, uni)
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	// k22: a company admin cannot change a payee party; a suspended or
	// closed company takes no change from its members.
	f := e.foundation(t, vknE)
	cur, _ := e.svc.GetOrgParty(ctxOf(t, fin), fin, org("club_a"), f.Party.ID)
	ca := actor(t, "club_admin")
	_, err = e.svc.UpdateOrgParty(ctxOf(t, ca), ca, org("club_a"), f.Party.ID, cur.ETag, parties.PartyPatch{Title: ptr("Başka Vakıf")})
	problemtest.WantProblem(t, err, http.StatusForbidden, "forbidden")
	for name, code := range map[string]string{"company_suspended": "orgs.org_suspended", "company_closed": "orgs.invalid_state"} {
		m := authz.Actor{Kind: authz.KindUser, UserID: authztest.ID(name + "_admin"), MFAVerified: true,
			Memberships: []authz.Membership{{OrgID: org(name), OrgKind: authz.OrgCompany, Role: authz.MemberCompanyAdmin}}}
		_, err := e.svc.CreateBuyerParty(ctxOf(t, m), m, org(name), buyerInput(vknD))
		if _, c := problemtest.StatusCode(err); c != code {
			t.Fatalf("%s: %v, want %s", name, err, code)
		}
	}
	// 06 H-54: no natural-person payee in the MVP.
	for _, lt := range []parties.LegalType{parties.LegalIndividual, parties.LegalSoleProp} {
		p := foundationInput(vknD)
		p.LegalType, p.TCKN = lt, ""
		if lt == parties.LegalIndividual {
			p.TaxNo, p.TCKN = "", tcknA
		}
		_, err := e.svc.AdminCreateParty(ctxOf(t, fin), fin, p)
		problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "parties.natural_person_payee")
	}
}

// TestDownloadDocument_Audited_PresignTTL: a document is served as a
// 5-minute URL of the files pipeline, every download is audited (before
// the URL is handed out), a file that is not clean is 409 and nothing is
// audited for a refused request.
func TestDownloadDocument_Audited_PresignTTL(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cma := actor(t, "company_admin")
	p := e.buyer(t, vknA)
	co := org("company_a")
	fileID := e.files.add(cma.UserID, &co)
	d, err := e.svc.UploadDocument(ctxOf(t, cma), cma, co, p.Party.ID, parties.DocumentInput{Kind: parties.DocTaxCertificate, FileID: fileID})
	if err != nil {
		t.Fatal(err)
	}
	if o := e.files.attached[fileID]; o.Module != "parties" || o.EntityID != d.ID || o.Purpose != files.PurposePartyDocument {
		t.Fatalf("attached as %+v", o)
	}
	app := newHTTP(t, e, nil)
	path := orgPath("company_a", idPath(p.Party.ID), "/documents/", d.ID.String(), "/download")
	for i, persona := range []string{"company_admin", "finance_admin"} {
		rec, out := call(t, app, persona, http.MethodGet, path, nil, nil)
		wantStatus(t, rec, http.StatusOK)
		if out["url"] != "https://files.koro.test/"+fileID.String() || out["expires_at"] != "2026-10-04T09:05:00Z" ||
			rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %v %v", persona, out, rec.Header())
		}
		ev := e.audit.Of(authz.PartiesDocumentRead)
		if len(ev) != i+1 {
			t.Fatalf("%d download events after %d downloads", len(ev), i+1)
		}
		last := ev[i]
		if last.ResourceType != authz.TypeLegalPartyDocument || last.ResourceID == nil || *last.ResourceID != d.ID ||
			last.OrgID == nil || *last.OrgID != co || last.ActorUserID == nil || *last.ActorUserID != authztest.ID(persona) ||
			last.Metadata["ttl_seconds"] != 300 || last.Metadata["legal_party_id"] != p.Party.ID.String() ||
			last.Metadata["file_id"] != fileID.String() {
			t.Fatalf("download audit = %+v", last)
		}
	}
	if !slices.Equal(e.files.ttls, []time.Duration{5 * time.Minute, 5 * time.Minute}) {
		t.Fatalf("presign TTLs = %v, want 5m each (04 §1.14)", e.files.ttls)
	}
	// No role: 403 before anything is read or audited; another org: 404.
	for persona, status := range map[string]int{"company_member": 403, "club_admin": 404, "moderator": 403} {
		rec, _ := call(t, app, persona, http.MethodGet, path, nil, nil)
		wantStatus(t, rec, status)
	}
	// IDOR: the document under another party of the same org is 404.
	other := e.buyer(t, vknC)
	rec, _ := call(t, app, "company_admin", http.MethodGet,
		orgPath("company_a", idPath(other.Party.ID), "/documents/", d.ID.String(), "/download"), nil, nil)
	wantStatus(t, rec, http.StatusNotFound)
	if n := len(e.audit.Of(authz.PartiesDocumentRead)); n != 2 {
		t.Fatalf("refused downloads were audited: %d events", n)
	}
	// The audit write fails: no URL.
	e.audit.Fail(errors.New("audit down"))
	if _, err := e.svc.DownloadDocument(ctxOf(t, cma), cma, co, p.Party.ID, d.ID); err == nil {
		t.Fatal("a download whose audit failed was handed out")
	}
	e.audit.Fail(nil)
	// A file that is not scanned clean is 409 files.not_clean.
	e.files.set(fileID, func(m *files.FileMeta) { m.ScanStatus = files.ScanPending })
	_, err = e.svc.DownloadDocument(ctxOf(t, cma), cma, co, p.Party.ID, d.ID)
	problemtest.WantProblem(t, err, http.StatusConflict, "files.not_clean")
}

// TestSeedPlatformParty_DraftIdempotent: `koro seed bootstrap-commerce`
// writes Koro's party as a draft once (T-31); a second run returns it
// unchanged, even after an admin filled it in.
func TestSeedPlatformParty_DraftIdempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sd, err := parties.NewSeeder(parties.SeederDeps{Store: e.store, Tx: db.NopTx{}, Audit: e.audit, Clock: e.clk, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Platform(t.Context()); !errors.Is(err, parties.ErrNotFound) {
		t.Fatalf("Platform before the seed: %v", err)
	}
	p, created, err := sd.EnsurePlatform(t.Context())
	if err != nil || !created || p.Kind != parties.KindPlatform || p.Status != parties.StatusDraft || p.Title != parties.PlatformDraftTitle ||
		p.OrgID != nil || p.CreatedBy != nil {
		t.Fatalf("first run: %+v created=%v err=%v", p, created, err)
	}
	ev := e.audit.Of(authz.PartiesLegalPartyCreate)
	if len(ev) != 1 || ev[0].SystemJob != parties.SeedJob || ev[0].ActorUserID != nil {
		t.Fatalf("seed audit = %+v", ev)
	}
	// Finance fills it in; the seed leaves it alone.
	fin := actor(t, "finance_admin")
	_, err = e.svc.AdminVerifyParty(ctxOf(t, fin), fin, p.ID, parties.Decision{Decision: parties.StatusVerified})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "parties.verification_incomplete")
	cur, err := e.svc.AdminUpdateParty(ctxOf(t, fin), fin, p.ID, `"`+strings.Repeat("0", 3)+`"`, parties.PartyPatch{Title: ptr("Koro Teknoloji A.Ş.")})
	problemtest.WantErr(t, err, parties.ErrPreconditionFailed)
	page, err := e.svc.AdminListParties(ctxOf(t, fin), fin, parties.AdminQuery{Kind: parties.KindPlatform})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("admin list of the platform party: %+v %v", page, err)
	}
	cur, err = e.svc.AdminUpdateParty(ctxOf(t, fin), fin, p.ID, page.Items[0].ETag, parties.PartyPatch{Title: ptr("Koro Teknoloji A.Ş."), TaxNo: ptr(vknE)})
	if err != nil || cur.Party.Status != parties.StatusDraft {
		t.Fatalf("filling in the draft: %+v %v", cur.Party, err)
	}
	again, created, err := sd.EnsurePlatform(t.Context())
	if err != nil || created || again.ID != p.ID || again.Title != "Koro Teknoloji A.Ş." || again.Status != parties.StatusDraft {
		t.Fatalf("second run: %+v created=%v err=%v", again, created, err)
	}
	if n := len(e.audit.Of(authz.PartiesLegalPartyCreate)); n != 1 {
		t.Fatalf("the second run was audited as a create (%d events)", n)
	}
	got, err := e.svc.Platform(t.Context())
	if err != nil || got.ID != p.ID {
		t.Fatalf("Platform = %+v %v", got, err)
	}
	// One platform party: a second one is 409; the platform party cannot be retired.
	_, err = e.svc.AdminCreateParty(ctxOf(t, fin), fin, parties.PartyInput{Kind: parties.KindPlatform, LegalType: parties.LegalCompany, Title: "Koro 2"})
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.platform_exists")
	_, err = e.svc.AdminUpdateParty(ctxOf(t, fin), fin, p.ID, cur.ETag, parties.PartyPatch{Deactivate: true})
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.invalid_state")
}

// TestLogs_NoTaxIDs: tax numbers, MERSİS numbers and national IDs never
// reach a log line (module and HTTP pipeline, plain handlers without the
// obs masks), an audit row or an error response, valid or not (05 §11.2).
func TestLogs_NoTaxIDs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, naturalPersons)
	httpLogs := &syncBuffer{}
	app := newHTTP(t, e, slog.New(slog.NewJSONHandler(httpLogs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	const badVKN, badTCKN, badMERSIS = "1234567891", "10000000147", "98765"
	var bodies []string
	do := func(persona, method, path string, body any, hdr map[string]string, want int) map[string]any {
		t.Helper()
		rec, out := call(t, app, persona, method, path, body, hdr)
		wantStatus(t, rec, want)
		if want >= 400 {
			bodies = append(bodies, rec.Body.String())
		}
		return out
	}
	buyer := map[string]any{
		"legal_type": "company", "title": "Örnek Yazılım A.Ş.", "tax_office": "Zincirlikuyu", "tax_no": vknA,
		"mersis_no": mersisA, "address": map[string]any{"line1": "Büyükdere Cad. 1", "city": "İstanbul"},
		"email": "fatura@ornek.test", "phone": "+905321234567",
	}
	out := do("company_admin", http.MethodPost, orgPath("company_a"), buyer, map[string]string{idempotency.HeaderKey: "create-buyer-1"}, 201)
	id := out["party_id"].(string)
	do("company_admin", http.MethodPost, orgPath("company_a"), buyer, map[string]string{idempotency.HeaderKey: "create-buyer-1"}, 201)
	buyer["tax_no"] = vknA
	do("other_company_member", http.MethodPost, orgPath("company_b"), buyer, nil, 409)
	buyer["tax_no"], buyer["mersis_no"] = badVKN, badMERSIS
	do("other_company_member", http.MethodPost, orgPath("company_b"), buyer, nil, 422)
	rec, _ := call(t, app, "company_admin", http.MethodGet, orgPath("company_a", "/"+id), nil, nil)
	etag := rec.Header().Get("ETag")
	do("company_admin", http.MethodPatch, orgPath("company_a", "/"+id), map[string]any{"tax_no": badVKN}, map[string]string{"If-Match": etag}, 422)
	do("company_admin", http.MethodPatch, orgPath("company_a", "/"+id), map[string]any{"tax_no": vknC}, map[string]string{"If-Match": etag}, 200)
	ind := map[string]any{"kind": "payee", "legal_type": "individual", "title": "Ali Veli", "tckn": tcknA,
		"address": map[string]any{"line1": "Moda Cad. 5", "city": "İstanbul"}}
	do("finance_admin", http.MethodPost, "/api/v1/admin/legal-parties", ind, nil, 201)
	do("finance_admin", http.MethodPost, "/api/v1/admin/legal-parties", ind, nil, 409)
	ind["tckn"] = badTCKN
	do("finance_admin", http.MethodPost, "/api/v1/admin/legal-parties", ind, nil, 422)
	do("finance_admin", http.MethodPost, "/api/v1/admin/legal-parties/search", map[string]any{"q": vknC}, nil, 200)
	do("platform_admin", http.MethodPost, "/api/v1/admin/legal-parties/"+id+"/verification", map[string]any{"decision": "verified"}, nil, 200)
	sd, err := parties.NewSeeder(parties.SeederDeps{Store: e.store, Tx: db.NopTx{}, Audit: e.audit, Clock: e.clk, Log: slog.New(slog.NewJSONHandler(e.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sd.EnsurePlatform(t.Context()); err != nil {
		t.Fatal(err)
	}

	secrets := []string{vknA, vknC, mersisA, tcknA, badVKN, badTCKN}
	if !strings.Contains(httpLogs.String(), `"route":"/api/v1/orgs/:org_id/legal-parties"`) {
		t.Fatalf("expected access log lines are missing:\n%s", httpLogs.String())
	}
	noDigits(t, "HTTP logs", httpLogs.String(), secrets...)
	noDigits(t, "module logs", e.logs.String(), secrets...)
	noDigits(t, "audit rows", dump(e.audit.All()), secrets...)
	noDigits(t, "error responses", strings.Join(bodies, "\n"), secrets...)
	for _, ev := range e.audit.All() {
		if ev.Action == string(authz.PartiesLegalPartyUpdate) && !slices.Contains(ev.Metadata["fields"].([]string), "tax_no") {
			t.Fatalf("update audit names the changed fields: %+v", ev.Metadata)
		}
	}
}
