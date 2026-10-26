package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// payoutsOf is the org's payouts as a collection (the ledger's org surface:
// Attrs present, empty).
func payoutsOf(org string) authz.Resource {
	id := authztest.ID(org)
	return authz.Resource{Type: authz.TypePayout, OrgID: &id,
		Attrs: map[string]string{authz.AttrStatus: "", authz.AttrCreatedBy: ""}}
}

// payoutOf is one payout of club_a for the approval (Attrs status,
// created_by; 03 §3.5).
func payoutOf(status, creator string) authz.Resource {
	org := authztest.ID("club_a")
	return authz.Resource{Type: authz.TypePayout, ID: uuid.NewV7(), OrgID: &org,
		Attrs: map[string]string{authz.AttrStatus: status, authz.AttrCreatedBy: authztest.ID(creator).String()}}
}

// TestLedgerPolicy_Matrix pins 03 §5.17 for the 18 personas (the HTTP
// matrix, testdata/authz/ledger.yaml, checks the same through the API):
// the admin-surface rows and the org-surface rows (S cells of club_admin
// and university_rep; members without the role 403, everyone else 404).
func TestLedgerPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	for _, c := range []struct {
		act  authz.Action
		want map[string]error // not listed: 403 (admin surface, never 404)
	}{
		{authz.LedgerJournalList, withAnon(map[string]error{"platform_admin": nil, "finance_admin": nil})},
		{authz.LedgerPayoutList, withAnon(map[string]error{"platform_admin": nil, "finance_admin": nil})},
		{authz.LedgerPayoutRead, withAnon(map[string]error{"platform_admin": nil, "finance_admin": nil})},
		{authz.LedgerPayableList, withAnon(map[string]error{"finance_admin": nil})},
		{authz.LedgerPayoutCreate, withAnon(map[string]error{"finance_admin": nil})},
		{authz.LedgerPayoutMarkPaid, withAnon(map[string]error{"finance_admin": nil})},
		{authz.LedgerPayoutMarkFailed, withAnon(map[string]error{"finance_admin": nil})},
		{authz.LedgerPayoutCancel, withAnon(map[string]error{"finance_admin": nil})},
	} {
		for _, p := range authztest.Personas() {
			w, ok := c.want[p.Name]
			if !ok {
				w = forbidden
			}
			wantErr(t, authz.Can(ctx, p.Actor, c.act, adminSurface), w, false)
		}
	}

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
	for _, c := range []struct {
		act authz.Action
		res func(org string) authz.Resource
	}{
		{authz.LedgerBalanceRead, payeesOf},
		{authz.LedgerPayoutList, payoutsOf},
		{authz.LedgerPayoutRead, payoutsOf},
	} {
		for _, o := range []struct {
			org  string
			want map[string]error // not listed: 404 (not a member)
		}{
			{"club_a", with(map[string]error{"club_member": forbidden, "club_admin": nil})},
			{"uni_a", with(map[string]error{"university_rep": nil})},
			{"company_a", with(map[string]error{"company_member": forbidden, "company_admin": forbidden})},
		} {
			for _, p := range authztest.Personas() {
				want, ok := o.want[p.Name]
				if !ok {
					want = notFound
				}
				err := authz.Can(ctx, p.Actor, c.act, c.res(o.org))
				if want == nil && err != nil || want != nil && err == nil {
					t.Errorf("%s %s %s: %v, want %v", c.act, o.org, p.Name, err, want)
					continue
				}
				if want != nil {
					wantErr(t, err, want, false)
				}
			}
		}
	}
}

// TestLedgerPolicy_PayoutFourEyes pins 03 §6.1 "Payout": only a finance
// admin approves, never the payout's creator; created_by is mandatory
// (06 H-27, fail closed).
func TestLedgerPolicy_PayoutFourEyes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, tc := range []struct {
		who  string
		r    authz.Resource
		want error
	}{
		{"finance_admin", payoutOf("pending_approval", "finance_admin"), authz.ErrFourEyes},
		{"finance_admin", payoutOf("pending_approval", "finance_admin_2"), nil},
		{"platform_admin", payoutOf("pending_approval", "finance_admin_2"), authz.ErrForbidden},
		{"support", payoutOf("pending_approval", "finance_admin_2"), authz.ErrForbidden},
		{"club_admin", payoutOf("pending_approval", "finance_admin_2"), authz.ErrNotFound},
	} {
		wantErr(t, authz.Can(ctx, persona(t, tc.who), authz.LedgerPayoutApprove, tc.r), tc.want, false)
	}
	noKey := payoutOf("pending_approval", "finance_admin_2")
	delete(noKey.Attrs, authz.AttrCreatedBy)
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.LedgerPayoutApprove, noKey), authz.ErrForbidden, true)
	empty := payoutOf("pending_approval", "finance_admin_2")
	empty.Attrs[authz.AttrCreatedBy] = ""
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.LedgerPayoutApprove, empty), authz.ErrForbidden, true)
	// The approval is decided on the payout only, never on the admin surface.
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.LedgerPayoutApprove, adminSurface), authz.ErrNotFound, false)
}

func TestLedgerPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// k-mfa: platform_admin and finance_admin need an MFA session, the
	// university rep too; a club admin does not.
	for _, name := range []string{"platform_admin", "finance_admin"} {
		a := persona(t, name)
		a.MFAVerified = false
		wantErr(t, authz.Can(ctx, a, authz.LedgerJournalList, adminSurface), authz.ErrMFARequired, false)
		wantErr(t, authz.Can(ctx, a, authz.LedgerPayoutRead, payoutsOf("club_a")), authz.ErrMFARequired, false)
	}
	fin := persona(t, "finance_admin")
	fin.MFAVerified = false
	wantErr(t, authz.Can(ctx, fin, authz.LedgerPayoutApprove, payoutOf("pending_approval", "finance_admin_2")),
		authz.ErrMFARequired, false)
	uni := persona(t, "university_rep")
	uni.MFAVerified = false
	wantErr(t, authz.Can(ctx, uni, authz.LedgerBalanceRead, payeesOf("uni_a")), authz.ErrMFARequired, false)
	cla := persona(t, "club_admin")
	cla.MFAVerified = false
	wantErr(t, authz.Can(ctx, cla, authz.LedgerBalanceRead, payeesOf("club_a")), nil, false)

	// The journal is bound to the admin surface only.
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.LedgerJournalList,
		authz.Resource{Type: authz.TypeLedger}), authz.ErrNotFound, false)
	// The balance is also a staff read on the admin surface (a payee's open receivable lines, BE-129),
	// like the IBAN reveal (pad, fin); the payout writes are admin-surface only.
	for _, act := range []authz.Action{authz.LedgerBalanceRead, authz.LedgerIbanReveal} {
		for _, name := range []string{"platform_admin", "finance_admin"} {
			wantErr(t, authz.Can(ctx, persona(t, name), act, adminSurface), nil, false)
		}
		for _, name := range []string{"club_admin", "support", "dpo"} {
			wantErr(t, authz.Can(ctx, persona(t, name), act, adminSurface), authz.ErrForbidden, false)
		}
	}
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.LedgerIbanReveal, payoutsOf("club_a")), authz.ErrNotFound, false)
	for _, act := range []authz.Action{authz.LedgerPayoutCreate, authz.LedgerPayoutMarkPaid, authz.LedgerPayoutCancel} {
		wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), act, payoutsOf("club_a")), authz.ErrNotFound, false)
	}

	// No system actor may list the journal or move a payout (03 §3.7).
	sys := authz.SystemActor("ledger.integrity_check")
	wantErr(t, authz.Can(ctx, sys, authz.LedgerJournalList, adminSurface), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, sys, authz.LedgerPayoutCreate, adminSurface), authz.ErrForbidden, false)
}
