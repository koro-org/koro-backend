package orgs_test

import (
	"context"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/identity"
)

// TestReauthDenied_EveryROperation (03 §6.2, BE-074): every orgs R
// operation (orgs.org.suspend: suspend and reactivate, orgs.org.close,
// orgs.ownership.transfer) audits a refused proof — wrong (403
// identity.reauth_failed) or missing (403 identity.reauth_required) — as
// one denied reauth_failed record of the org. Close and transfer read the
// org first, so their record is scoped to it; suspend and reactivate check
// the proof before reading, so theirs is not (06 H-38: never from the
// request) and names the transition only. Nothing is written when identity
// cannot answer within the request budget (the bare deadline, httpx 503);
// the right proof goes through afterwards. Before BE-074 the refusal went
// back without a record.
func TestReauthDenied_EveryROperation(t *testing.T) {
	t.Parallel()
	type op struct {
		actor  authz.Actor
		call   func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error
		denied audit.Event
	}
	verified := func(t *testing.T, e *env) uuid.UUID {
		v, _ := e.company(t, "Askıya Alınan Şirket")
		if _, err := e.svc.AdminVerifyOrg(t.Context(), pad(t), v.Org.ID, ""); err != nil {
			t.Fatal(err)
		}
		return v.Org.ID
	}
	cases := []struct {
		name   string
		action authz.Action
		setup  func(t *testing.T, e *env) op
	}{
		{name: "suspendOrg", action: authz.OrgsOrgSuspend, setup: func(t *testing.T, e *env) op {
			id := verified(t, e)
			return op{actor: pad(t),
				call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
					_, err := e.svc.AdminSuspendOrg(ctx, a, id, "Şikâyet incelemesi", p)
					return err
				},
				denied: audit.Event{ResourceID: &id, Metadata: map[string]any{"transition": "suspend"}}}
		}},
		{name: "reactivateOrg", action: authz.OrgsOrgSuspend, setup: func(t *testing.T, e *env) op {
			id := verified(t, e)
			if _, err := e.svc.AdminSuspendOrg(t.Context(), pad(t), id, "Şikâyet incelemesi", proof(goodPassword)); err != nil {
				t.Fatal(err)
			}
			return op{actor: pad(t),
				call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
					_, err := e.svc.AdminReactivateOrg(ctx, a, id, "İnceleme bitti", p)
					return err
				},
				denied: audit.Event{ResourceID: &id, Metadata: map[string]any{"transition": "reactivate"}}}
		}},
		{name: "closeOrg", action: authz.OrgsOrgClose, setup: func(t *testing.T, e *env) op {
			v, owner := e.company(t, "Kapanacak Şirket")
			id := v.Org.ID
			return op{actor: owner,
				call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
					_, err := e.svc.CloseOrg(ctx, a, id, "Faaliyet sona erdi", p)
					return err
				},
				denied: audit.Event{ResourceID: &id, OrgID: &id}}
		}},
		{name: "transferOrgOwnership", action: authz.OrgsOwnershipTransfer, setup: func(t *testing.T, e *env) op {
			v, owner := e.company(t, "Devir Şirketi")
			id := v.Org.ID
			target := e.member(t, id, authz.MemberCompanyAdmin).UserID
			return op{actor: owner,
				call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
					_, err := e.svc.TransferOwnership(ctx, a, id, target, p)
					return err
				},
				denied: audit.Event{ResourceID: &id, OrgID: &id}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			o := tc.setup(t, e)
			o.denied.Action, o.denied.ResourceType = string(tc.action), authz.TypeOrg
			audittest.ReauthCase[identity.ReauthProof]{
				Call: func(ctx context.Context, p identity.ReauthProof) error {
					return o.call(authz.WithActor(ctx, o.actor), o.actor, p)
				},
				Rows:   func() []audittest.Row { return e.audit.Rows(tc.action) },
				Denied: audittest.RowOf(o.denied.By(o.actor)), Late: e.late,
				Wrong: proof("yanlis-parola-1"), WrongErr: identity.ErrReauthFailed,
				Missing: proof(""), MissingErr: identity.ErrReauthRequired,
				Good: proof(goodPassword),
			}.Run(t)
		})
	}
}
