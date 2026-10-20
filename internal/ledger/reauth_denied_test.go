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
		Rows: func() []audittest.Row { return e.audit.reauthRows(authz.LedgerPayoutApprove) },
		Denied: rowOf(audit.Event{Action: string(authz.LedgerPayoutApprove), OrgID: &org, ResourceType: authz.TypePayout,
			ResourceID: &id}.By(a)),
		Late:  e.reauth,
		Wrong: identity.ReauthProof{Password: "wrong-password"}, WrongErr: identity.ErrReauthFailed,
		Missing: identity.ReauthProof{}, MissingErr: identity.ErrReauthRequired,
		Good: identity.ReauthProof{Password: reauthPassword},
	}.Run(t)
}

// rowOf is e in audittest's terms.
func rowOf(e audit.Event) audittest.Row {
	return audittest.Row{Action: e.Action, Outcome: string(e.Outcome), Reason: e.Reason, ActorUserID: e.ActorUserID,
		ActorOrgID: e.ActorOrgID, SystemJob: e.SystemJob, OrgID: e.OrgID, ResourceType: e.ResourceType, ResourceID: e.ResourceID,
		Metadata: e.Metadata}
}

// reauthRows returns the recorded events of act in audittest's terms.
func (l *auditLog) reauthRows(act authz.Action) []audittest.Row {
	var out []audittest.Row
	for _, e := range l.of(act) {
		out = append(out, rowOf(e))
	}
	return out
}
