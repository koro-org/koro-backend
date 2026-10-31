package parties_test

import (
	"context"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
)

// TestReauthDenied_EveryROperation (03 §6.2, BE-074): every parties R
// operation (parties.bank_account.submit by the org and by an admin, and
// the org's evidence replacement (BE-154b), approve, revoke) audits a refused proof — wrong (403
// identity.reauth_failed) or missing (403 identity.reauth_required) — as
// one denied reauth_failed record of the bank account (none yet for a
// submission), scoped to the payee's org like the account's own records
// and naming the payee; no IBAN, masked or not: nothing happened. Nothing
// is written when identity cannot answer within the request budget (the
// bare deadline, httpx 503); the right proof goes through afterwards.
// Before BE-074 the refusal went back without a record.
func TestReauthDenied_EveryROperation(t *testing.T) {
	t.Parallel()
	type op struct {
		persona string
		call    func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error
		account *uuid.UUID
	}
	cases := []struct {
		name   string
		action authz.Action
		setup  func(t *testing.T, e *env, payee uuid.UUID) op
	}{
		{name: "submitPayeeBankAccount", action: authz.PartiesBankAccountSubmit,
			setup: func(t *testing.T, e *env, payee uuid.UUID) op {
				in := e.accountInput(t, ibanA, "club_admin")
				return op{persona: "club_admin", call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
					_, err := e.svc.SubmitPayeeBankAccount(ctx, a, org("club_a"), payee, in, p)
					return err
				}}
			}},
		{name: "adminSubmitBankAccount", action: authz.PartiesBankAccountSubmit,
			setup: func(t *testing.T, e *env, payee uuid.UUID) op {
				in := e.accountInput(t, ibanA, "finance_admin")
				return op{persona: "finance_admin", call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
					_, err := e.svc.AdminSubmitBankAccount(ctx, a, payee, in, p)
					return err
				}}
			}},
		{name: "adminApproveBankAccount", action: authz.PartiesBankAccountApprove,
			setup: func(t *testing.T, e *env, payee uuid.UUID) op {
				id := e.orgSubmit(t, payee, ibanA).Account.ID
				if _, err := e.verifyAccount(t, "finance_admin", id); err != nil {
					t.Fatal(err)
				}
				return op{persona: "platform_admin", account: &id, call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
					_, err := e.svc.AdminApproveBankAccount(ctx, a, id, p)
					return err
				}}
			}},
		{name: "replacePayeeBankAccountEvidence", action: authz.PartiesBankAccountSubmit,
			setup: func(t *testing.T, e *env, payee uuid.UUID) op {
				b := e.orgSubmit(t, payee, ibanA).Account
				e.infected(b.EvidenceFileID)
				club := org("club_a")
				next := e.files.addPurpose(files.PurposeBankAccountEvidence, actor(t, "club_admin").UserID, &club)
				return op{persona: "club_admin", account: &b.ID, call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
					_, err := e.svc.ReplaceBankAccountEvidence(ctx, a, club, payee, b.ID, next, p)
					return err
				}}
			}},
		{name: "adminRevokeBankAccount", action: authz.PartiesBankAccountRevoke,
			setup: func(t *testing.T, e *env, payee uuid.UUID) op {
				id := e.approved(t, payee, ibanA).Account.ID
				return op{persona: "finance_admin", account: &id, call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
					_, err := e.svc.AdminRevokeBankAccount(ctx, a, id, "Hesap kapandı", p)
					return err
				}}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			payee := e.payee(t, vknC).Payee
			o := tc.setup(t, e, payee.ID)
			a, club := actor(t, o.persona), payee.OrgID
			audittest.ReauthCase[identity.ReauthProof]{
				Call: func(ctx context.Context, p identity.ReauthProof) error {
					return o.call(authz.WithActor(ctx, a), a, p)
				},
				Rows: func() []audittest.Row { return e.audit.Rows(tc.action) },
				Denied: audittest.RowOf(audit.Event{Action: string(tc.action), OrgID: &club, ResourceType: authz.TypeBankAccount, ResourceID: o.account,
					Metadata: map[string]any{"payee_id": payee.ID.String()}}.By(a)),
				Late:  e.late,
				Wrong: identity.ReauthProof{Password: "yanlış-parola"}, WrongErr: identity.ErrReauthFailed,
				Missing: identity.ReauthProof{}, MissingErr: identity.ErrReauthRequired,
				Good: proof(),
			}.Run(t)
		})
	}
}
