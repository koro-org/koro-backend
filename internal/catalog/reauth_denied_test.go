package catalog_test

import (
	"context"
	"testing"
	"time"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/identity"
)

// TestReauthDenied_EveryROperation (03 §6.2, BE-074): both catalog R
// operations (catalog.policy.manage: create and close a commerce policy)
// audit a refused proof — wrong (403 identity.reauth_failed) or missing
// (403 identity.reauth_required) — as one denied reauth_failed record of
// the policy and the change (no rates: nothing happened); they write
// nothing when identity cannot answer within the request budget (the bare
// deadline, httpx 503) and go through with the right proof afterwards.
// Before BE-074 catalog returned the refusal without a record.
func TestReauthDenied_EveryROperation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		persona string
		setup   func(t *testing.T, e *env) (call func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error, denied audit.Event)
	}{
		{name: "createCommercePolicy", persona: "finance_admin",
			setup: func(t *testing.T, e *env) (func(context.Context, authz.Actor, identity.ReauthProof) error, audit.Event) {
				return func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
					_, err := e.svc.AdminCreatePolicy(ctx, a, catalog.DefaultPolicy(1000), p)
					return err
				}, audit.Event{Metadata: map[string]any{"change": "created"}}
			}},
		{name: "closeCommercePolicy", persona: "platform_admin",
			setup: func(t *testing.T, e *env) (func(context.Context, authz.Actor, identity.ReauthProof) error, audit.Event) {
				p := e.policy(t, catalog.ScopeDefault, nil, 1000, t0.Add(-time.Hour), nil)
				id := p.ID
				return func(ctx context.Context, a authz.Actor, proof identity.ReauthProof) error {
					_, err := e.svc.AdminClosePolicy(ctx, a, p.ID, nil, proof)
					return err
				}, audit.Event{ResourceID: &id, Metadata: map[string]any{"change": "closed"}}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			call, denied := tc.setup(t, e)
			a := actor(t, tc.persona)
			denied.Action, denied.ResourceType = string(authz.CatalogPolicyManage), "commerce_policy"
			audittest.ReauthCase[identity.ReauthProof]{
				Call: func(ctx context.Context, p identity.ReauthProof) error {
					return call(authz.WithActor(ctx, a), a, p)
				},
				Rows:   func() []audittest.Row { return e.audit.Rows(authz.CatalogPolicyManage) },
				Denied: audittest.RowOf(denied.By(a)), Late: e.late,
				Wrong: identity.ReauthProof{Password: "wrong-password"}, WrongErr: identity.ErrReauthFailed,
				Missing: identity.ReauthProof{}, MissingErr: identity.ErrReauthRequired,
				Good: proof(),
			}.Run(t)
		})
	}
}
