package authz_test

import (
	"testing"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// TestLedgerPolicy_Matrix pins 03 §5.17 ledger.journal.list for the 18
// personas (the HTTP matrix, testdata/authz/ledger.yaml, checks the same
// through the API).
func TestLedgerPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	want := withAnon(map[string]error{"platform_admin": nil, "finance_admin": nil})
	for _, p := range authztest.Personas() {
		w, ok := want[p.Name]
		if !ok {
			w = authz.ErrForbidden // admin surface: 403, never 404
		}
		wantErr(t, authz.Can(ctx, p.Actor, authz.LedgerJournalList, adminSurface), w, false)
	}
}

func TestLedgerPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// k-mfa: platform_admin and finance_admin need an MFA session.
	for _, name := range []string{"platform_admin", "finance_admin"} {
		a := persona(t, name)
		a.MFAVerified = false
		wantErr(t, authz.Can(ctx, a, authz.LedgerJournalList, adminSurface), authz.ErrMFARequired, false)
	}

	// The journal is bound to the admin surface only.
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.LedgerJournalList,
		authz.Resource{Type: authz.TypeLedger}), authz.ErrNotFound, false)

	// The BE-044 actions stay default deny, staff included.
	for _, act := range []authz.Action{
		authz.LedgerBalanceRead, authz.LedgerPayableList, authz.LedgerPayoutList, authz.LedgerPayoutRead,
		authz.LedgerPayoutCreate, authz.LedgerPayoutApprove, authz.LedgerPayoutMarkPaid,
		authz.LedgerPayoutMarkFailed, authz.LedgerPayoutCancel,
	} {
		err := authz.Can(ctx, persona(t, "finance_admin"), act, adminSurface)
		if err == nil {
			t.Errorf("%s: finance_admin allowed before BE-044", act)
		}
	}

	// No system actor may list the journal (03 §3.7).
	wantErr(t, authz.Can(ctx, authz.SystemActor("ledger.integrity_check"), authz.LedgerJournalList, adminSurface),
		authz.ErrForbidden, false)
}
