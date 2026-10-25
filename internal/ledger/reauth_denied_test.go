package ledger_test

import (
	"context"
	"testing"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/identity"
)

// TestReauthDenied_EveryROperation (03 §6.2, BE-074): ledger's R operation
// (ledger.payout.approve, the second eye) audits a refused proof — wrong
// (403 identity.reauth_failed) or missing (403 identity.reauth_required) —
// as one denied reauth_failed record of the payout, scoped to its org (no
// IBAN, no amount: nothing happened); it writes nothing when identity
// cannot answer within the request budget (the bare deadline, httpx 503)
// and approves with the right proof afterwards. Before BE-074 the refusal
// went back without a record.
func TestReauthDenied_EveryROperation(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	payee := e.seller(t, "club_a")
	e.payable(t, payee)
	p := e.create(t, "finance_admin_2", payee)
	a := actor(t, "finance_admin")
	id, org := p.ID, p.OrgID
	audittest.ReauthCase[identity.ReauthProof]{
		Call: func(ctx context.Context, proof identity.ReauthProof) error {
			_, err := e.svc.AdminApprovePayout(authz.WithActor(ctx, a), a, p.ID, proof)
			return err
		},
		Rows: func() []audittest.Row { return e.audit.Rows(authz.LedgerPayoutApprove) },
		Denied: audittest.RowOf(audit.Event{Action: string(authz.LedgerPayoutApprove), OrgID: &org, ResourceType: authz.TypePayout,
			ResourceID: &id}.By(a)),
		Late:  e.reauth,
		Wrong: identity.ReauthProof{Password: "wrong-password"}, WrongErr: identity.ErrReauthFailed,
		Missing: identity.ReauthProof{}, MissingErr: identity.ErrReauthRequired,
		Good: identity.ReauthProof{Password: reauthPassword},
	}.Run(t)
}
