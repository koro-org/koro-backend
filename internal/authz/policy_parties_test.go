package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// partyOf is the Resource the parties service builds on an org surface
// (BE-036): OrgID = the org of the path; Attrs kind and scope from the rows.
func partyOf(org, kind, scope string) authz.Resource {
	id := authztest.ID(org)
	return authz.Resource{Type: authz.TypeLegalParty, ID: uuid.NewV7(), OrgID: &id,
		Attrs: map[string]string{"kind": kind, "scope": scope}}
}

// documentOf is a legal_party_documents row for its verification.
func documentOf(uploader string) authz.Resource {
	return authz.Resource{Type: authz.TypeLegalPartyDocument, ID: uuid.NewV7(),
		Attrs: map[string]string{authz.AttrUploadedBy: authztest.ID(uploader).String()}}
}

// TestPartiesPolicy_Matrix pins 03 §5.13 (the BE-036 rows) for the 18
// personas: org surface (S, k21, k22) and admin surface (pad, fin). The
// HTTP matrix (testdata/authz/parties.yaml) checks the same through the API.
func TestPartiesPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	staff := map[string]error{
		"anonymous": forbidden, "moderator": forbidden, "support": forbidden, "dpo": forbidden, "platform_admin": nil,
		"finance_admin": nil, "talent_auditor": forbidden,
	}
	with := func(m map[string]error) map[string]error {
		out := map[string]error{}
		for k, v := range staff {
			out[k] = v
		}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	type cell struct {
		act  authz.Action
		r    authz.Resource
		want map[string]error // personas not listed: 404 (not a member of the org)
	}
	cells := []cell{
		// company_a: the buyer party (scope org). cmm S, cma S for read; cma k22 for create/update;
		// cma S for documents; cmm has no document cell.
		{authz.PartiesLegalPartyRead, partyOf("company_a", "buyer_company", authz.PartyScopeOrg),
			with(map[string]error{"company_member": nil, "company_admin": nil})},
		{authz.PartiesLegalPartyRead, partyOf("company_a", "", authz.PartyScopeList),
			with(map[string]error{"company_member": nil, "company_admin": nil})},
		{authz.PartiesLegalPartyCreate, partyOf("company_a", "buyer_company", authz.PartyScopeOrg),
			with(map[string]error{"company_member": forbidden, "company_admin": nil})},
		{authz.PartiesLegalPartyUpdate, partyOf("company_a", "buyer_company", authz.PartyScopeOrg),
			with(map[string]error{"company_member": forbidden, "company_admin": nil})},
		{authz.PartiesDocumentUpload, partyOf("company_a", "buyer_company", authz.PartyScopeOrg),
			with(map[string]error{"company_member": forbidden, "company_admin": nil})},
		{authz.PartiesDocumentRead, partyOf("company_a", "buyer_company", authz.PartyScopeOrg),
			with(map[string]error{"company_member": forbidden, "company_admin": nil})},
		// club_a: the foundation paid for the club (scope payee, k21).
		{authz.PartiesLegalPartyRead, partyOf("club_a", "payee", authz.PartyScopePayee),
			with(map[string]error{"club_member": forbidden, "club_admin": nil})},
		{authz.PartiesLegalPartyRead, partyOf("club_a", "", authz.PartyScopeList),
			with(map[string]error{"club_member": forbidden, "club_admin": nil})},
		{authz.PartiesDocumentUpload, partyOf("club_a", "payee", authz.PartyScopePayee),
			with(map[string]error{"club_member": forbidden, "club_admin": nil})},
		{authz.PartiesDocumentRead, partyOf("club_a", "payee", authz.PartyScopePayee),
			with(map[string]error{"club_member": forbidden, "club_admin": nil})},
		// k22: club admins never create or change a party, a payee party is fin/pad's.
		{authz.PartiesLegalPartyUpdate, partyOf("club_a", "payee", authz.PartyScopePayee),
			with(map[string]error{"club_member": forbidden, "club_admin": forbidden})},
		{authz.PartiesLegalPartyCreate, partyOf("club_a", "buyer_company", authz.PartyScopeOrg),
			with(map[string]error{"club_member": forbidden, "club_admin": forbidden})},
		// uni_a: university_rep S·k21 (k-cs: never career_staff).
		{authz.PartiesLegalPartyRead, partyOf("uni_a", "payee", authz.PartyScopePayee),
			with(map[string]error{"university_rep": nil})},
		{authz.PartiesDocumentRead, partyOf("uni_a", "payee", authz.PartyScopePayee),
			with(map[string]error{"university_rep": nil})},
		// k21: the university's own party without a payee row is not its payee side.
		{authz.PartiesLegalPartyRead, partyOf("uni_a", "payee", authz.PartyScopeOrg),
			with(map[string]error{"university_rep": forbidden})},
	}
	for _, c := range cells {
		for _, p := range authztest.Personas() {
			want, ok := c.want[p.Name]
			if !ok {
				want = notFound
			}
			err := authz.Can(ctx, p.Actor, c.act, c.r)
			if want == nil && err != nil || want != nil && err == nil {
				t.Errorf("%s scope=%s kind=%s %s: %v, want %v", c.act, c.r.Attr("scope"), c.r.Attr("kind"), p.Name, err, want)
				continue
			}
			if want != nil {
				wantErr(t, err, want, false)
			}
		}
	}
	// Admin surface: 403 before reading for everyone without the role.
	for _, act := range []authz.Action{authz.PartiesLegalPartyRead, authz.PartiesLegalPartyCreate, authz.PartiesLegalPartyUpdate,
		authz.PartiesLegalPartyVerify, authz.PartiesDocumentVerify} {
		for _, p := range authztest.Personas() {
			var want error = forbidden
			if p.Name == "platform_admin" || p.Name == "finance_admin" {
				want = nil
			}
			wantErr(t, authz.Can(ctx, p.Actor, act, adminSurface), want, false)
		}
	}
}

func TestPartiesPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	buyerA := partyOf("company_a", "buyer_company", authz.PartyScopeOrg)

	// company_owner counts as company_admin (S, k22).
	wantErr(t, authz.Can(ctx, persona(t, "company_owner_a"), authz.PartiesLegalPartyUpdate, buyerA), nil, false)

	// k22: a company admin changes only buyer parties of its own company.
	payeeAtCompany := partyOf("company_a", "payee", authz.PartyScopeOrg)
	wantErr(t, authz.Can(ctx, persona(t, "company_admin"), authz.PartiesLegalPartyUpdate, payeeAtCompany), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, persona(t, "company_admin"), authz.PartiesLegalPartyUpdate,
		partyOf("company_a", "buyer_company", authz.PartyScopePayee)), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.PartiesLegalPartyUpdate, payeeAtCompany), nil, false)

	// k-mfa: company_admin, university_rep, platform_admin and finance_admin need an MFA
	// session; company_member (recruiter) and club_admin do not.
	for _, tc := range []struct {
		name string
		r    authz.Resource
		act  authz.Action
		want error
	}{
		{"company_admin", buyerA, authz.PartiesLegalPartyCreate, authz.ErrMFARequired},
		{"university_rep", partyOf("uni_a", "payee", authz.PartyScopePayee), authz.PartiesLegalPartyRead, authz.ErrMFARequired},
		{"finance_admin", buyerA, authz.PartiesLegalPartyRead, authz.ErrMFARequired},
		{"platform_admin", adminSurface, authz.PartiesLegalPartyVerify, authz.ErrMFARequired},
		{"company_member", buyerA, authz.PartiesLegalPartyRead, nil},
		{"club_admin", partyOf("club_a", "payee", authz.PartyScopePayee), authz.PartiesDocumentUpload, nil},
	} {
		a := persona(t, tc.name)
		a.MFAVerified = false
		wantErr(t, authz.Can(ctx, a, tc.act, tc.r), tc.want, false)
	}

	// Document verification: four eyes against the uploader (06 H-19 kind),
	// platform roles only; the uploaded_by key is mandatory.
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.PartiesDocumentVerify, documentOf("club_admin")), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.PartiesDocumentVerify, documentOf("platform_admin")),
		authz.ErrFourEyes, false)
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.PartiesDocumentVerify, documentOf("platform_admin")), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.PartiesDocumentVerify, documentOf("company_admin")),
		authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "moderator"), authz.PartiesDocumentVerify, documentOf("club_admin")),
		authz.ErrForbidden, false)
	noKey := authz.Resource{Type: authz.TypeLegalPartyDocument, ID: uuid.NewV7(), Attrs: map[string]string{}}
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.PartiesDocumentVerify, noKey), authz.ErrForbidden, true)

	// A legal party without the kind Attrs key is a programmer error.
	org := authztest.ID("company_a")
	noKind := authz.Resource{Type: authz.TypeLegalParty, OrgID: &org, Attrs: map[string]string{"scope": authz.PartyScopeOrg}}
	wantErr(t, authz.Can(ctx, persona(t, "company_admin"), authz.PartiesLegalPartyRead, noKind), authz.ErrForbidden, true)

	// An org resource without OrgID is nobody's surface but the staff's.
	bare := authz.Resource{Type: authz.TypeLegalParty, Attrs: map[string]string{"kind": "payee", "scope": authz.PartyScopePayee}}
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.PartiesLegalPartyRead, bare), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.PartiesLegalPartyRead, bare), nil, false)

	// Org actions are bound to their resource types, admin-only ones to the admin surface.
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.PartiesLegalPartyVerify, buyerA), authz.ErrNotFound, false)
	// T-234: documents on the admin surface (staff only).
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.PartiesDocumentUpload, adminSurface), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.PartiesDocumentRead, adminSurface), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.PartiesDocumentRead, adminSurface), authz.ErrForbidden, false)
	// Org-only payee actions have no admin-surface cell.
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.PartiesPayeeList, adminSurface), authz.ErrForbidden, false)
}

// payeesOf is the payees collection of an org surface (BE-037).
func payeesOf(org string) authz.Resource {
	id := authztest.ID(org)
	return authz.Resource{Type: authz.TypePayee, OrgID: &id}
}

// accountOf is a bank account for the eyes (Attrs status, submitted_by,
// verified_by; 03 §3.5).
func accountOf(status, submitter, verifier string) authz.Resource {
	org := authztest.ID("club_a")
	v := ""
	if verifier != "" {
		v = authztest.ID(verifier).String()
	}
	return authz.Resource{Type: authz.TypeBankAccount, ID: uuid.NewV7(), OrgID: &org, Attrs: map[string]string{
		authz.AttrStatus: status, authz.AttrSubmittedBy: authztest.ID(submitter).String(), authz.AttrVerifiedBy: v,
	}}
}

// TestPartiesPolicy_PayeesMatrix pins the BE-037 rows of 03 §5.13 for the
// 18 personas: the payee org surface (cla S, uni S; pad, fin) and the admin
// surface (pad, fin).
func TestPartiesPolicy_PayeesMatrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	staffCells := map[string]error{
		"anonymous": forbidden, "moderator": forbidden, "support": forbidden, "dpo": forbidden, "platform_admin": nil,
		"finance_admin": nil, "talent_auditor": forbidden,
	}
	with := func(m map[string]error) map[string]error {
		out := map[string]error{}
		for k, v := range staffCells {
			out[k] = v
		}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	orgActs := []authz.Action{authz.PartiesPayeeList, authz.PartiesPayeeRead, authz.PartiesBankAccountList,
		authz.PartiesBankAccountSubmit}
	for _, act := range orgActs {
		for _, c := range []struct {
			org  string
			want map[string]error // not listed: 404 (not a member)
		}{
			{"club_a", with(map[string]error{"club_member": forbidden, "club_admin": nil})},
			{"uni_a", with(map[string]error{"university_rep": nil})},
			{"company_a", with(map[string]error{"company_member": forbidden, "company_admin": forbidden})},
		} {
			for _, p := range authztest.Personas() {
				want, ok := c.want[p.Name]
				if !ok {
					want = notFound
				}
				err := authz.Can(ctx, p.Actor, act, payeesOf(c.org))
				if want == nil && err != nil || want != nil && err == nil {
					t.Errorf("%s %s %s: %v, want %v", act, c.org, p.Name, err, want)
					continue
				}
				if want != nil {
					wantErr(t, err, want, false)
				}
			}
		}
	}
	// verify and approve are decided on the account (four eyes); their /admin
	// operations are gated with bank_account.list before reading.
	adminActs := []authz.Action{authz.PartiesPayeeManage, authz.PartiesPayeeVerify, authz.PartiesBankAccountList,
		authz.PartiesBankAccountSubmit, authz.PartiesBankAccountReject, authz.PartiesBankAccountRevoke,
		authz.PartiesDocumentUpload, authz.PartiesDocumentRead}
	for _, act := range adminActs {
		for _, p := range authztest.Personas() {
			var want error = forbidden
			if p.Name == "platform_admin" || p.Name == "finance_admin" {
				want = nil
			}
			wantErr(t, authz.Can(ctx, p.Actor, act, adminSurface), want, false)
		}
	}
}

// TestPartiesPolicy_BankAccountFourEyes pins 03 §6.1 "Payee IBAN": the
// first eye is never the submitter, the second eye neither the submitter
// nor the first eye (Y6); only finance and platform admins are eyes; the
// Attrs keys are mandatory (06 H-27).
func TestPartiesPolicy_BankAccountFourEyes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fe := authz.ErrFourEyes
	for _, tc := range []struct {
		who  string
		act  authz.Action
		r    authz.Resource
		want error
	}{
		{"finance_admin", authz.PartiesBankAccountVerify, accountOf("pending_verification", "club_admin", ""), nil},
		{"platform_admin", authz.PartiesBankAccountVerify, accountOf("pending_verification", "platform_admin", ""), fe},
		{"finance_admin", authz.PartiesBankAccountApprove, accountOf("pending_approval", "club_admin", "finance_admin"), fe},
		{"platform_admin", authz.PartiesBankAccountApprove, accountOf("pending_approval", "club_admin", "finance_admin"), nil},
		{"finance_admin", authz.PartiesBankAccountApprove, accountOf("pending_approval", "finance_admin", ""), fe},
		{"platform_admin", authz.PartiesBankAccountApprove, accountOf("pending_approval", "finance_admin", ""), nil},
		{"club_admin", authz.PartiesBankAccountApprove, accountOf("pending_approval", "finance_admin", ""), authz.ErrNotFound},
		{"moderator", authz.PartiesBankAccountVerify, accountOf("pending_verification", "club_admin", ""), authz.ErrForbidden},
		// reject and revoke are admin-surface actions only.
		{"finance_admin", authz.PartiesBankAccountRevoke, accountOf("approved", "club_admin", "platform_admin"), authz.ErrNotFound},
	} {
		wantErr(t, authz.Can(ctx, persona(t, tc.who), tc.act, tc.r), tc.want, false)
	}
	// A missing submitted_by key, or an empty one, is a programmer error (fail closed).
	noKey := accountOf("pending_approval", "club_admin", "")
	delete(noKey.Attrs, authz.AttrSubmittedBy)
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.PartiesBankAccountApprove, noKey), authz.ErrForbidden, true)
	empty := accountOf("pending_approval", "club_admin", "")
	empty.Attrs[authz.AttrSubmittedBy] = ""
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.PartiesBankAccountApprove, empty), authz.ErrForbidden, true)

	// MFA: the eyes are k-mfa role paths; a club admin's submission is not.
	fin := persona(t, "finance_admin")
	fin.MFAVerified = false
	wantErr(t, authz.Can(ctx, fin, authz.PartiesBankAccountVerify, accountOf("pending_verification", "club_admin", "")),
		authz.ErrMFARequired, false)
	wantErr(t, authz.Can(ctx, fin, authz.PartiesBankAccountList, adminSurface), authz.ErrMFARequired, false)
	cla := persona(t, "club_admin")
	cla.MFAVerified = false
	wantErr(t, authz.Can(ctx, cla, authz.PartiesBankAccountSubmit, payeesOf("club_a")), nil, false)

	// 06 H-60: the ownership correction is a platform admin's decision.
	own := authz.Resource{Type: authz.TypeLegalPartyOwnership, ID: uuid.NewV7()}
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.PartiesLegalPartyVerify, own), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.PartiesLegalPartyVerify, own), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.PartiesLegalPartyVerify, own), authz.ErrNotFound, false)
}
