package billing_test

import (
	"context"
	"testing"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/billing"
	"koro/internal/identity"
)

// TestReauthDenied_EveryROperation (03 §6.2, BE-074): billing's R
// operation (billing.integration.manage: PUT an integration's credentials)
// audits a refused proof — wrong (403 identity.reauth_failed) or missing
// (403 identity.reauth_required) — as one denied reauth_failed record of
// the integration naming the provider only (never a token: nothing was
// stored); it writes nothing when identity cannot answer within the
// request budget (the bare deadline, httpx 503) and stores the sealed
// tokens with the right proof afterwards. Before BE-074 the refusal went
// back without a record.
func TestReauthDenied_EveryROperation(t *testing.T) {
	t.Parallel()
	for _, persona := range []string{"finance_admin", "platform_admin"} {
		t.Run(persona, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			a := actor(t, persona)
			in := billing.IntegrationInput{ExternalAccount: "test-company-1", AccessToken: fakeAccessToken, RefreshToken: fakeRefreshToken}
			audittest.ReauthCase[identity.ReauthProof]{
				Call: func(ctx context.Context, p identity.ReauthProof) error {
					_, err := e.svc.AdminPutIntegration(authz.WithActor(ctx, a), a, "parasut", in, p)
					return err
				},
				Rows: func() []audittest.Row { return e.audit.Rows(authz.BillingIntegrationManage) },
				Denied: audittest.RowOf(audit.Event{Action: string(authz.BillingIntegrationManage), ResourceType: "integration",
					Metadata: map[string]any{"provider": "parasut"}}.By(a)),
				Late:  e.reauth,
				Wrong: identity.ReauthProof{Password: "wrong-password-not-real"}, WrongErr: identity.ErrReauthFailed,
				Missing: identity.ReauthProof{}, MissingErr: identity.ErrReauthRequired,
				Good: identity.ReauthProof{Password: reauthPassword},
			}.Run(t)
		})
	}
}
