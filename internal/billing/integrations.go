package billing

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/problem"
)

// Envelope purposes of the integration tokens: the purpose binds a sealed
// value to its column and provider (crypto.rewrap rebuilds it as prefix +
// provider).
const (
	AccessTokenPurposePrefix  = "billing.integration_credentials.access_token:"
	RefreshTokenPurposePrefix = "billing.integration_credentials.refresh_token:"
)

// Limits of adminPutIntegration.
const (
	maxTokenBytes      = 8 << 10 // an OAuth token, far above Paraşüt's
	maxExternalAccount = 100
)

// Integration is an integration's public state (04 §2.18
// adminListIntegrations): never a token.
type Integration struct {
	Provider        string
	ExternalAccount string
	HasAccessToken  bool
	HasRefreshToken bool
	ExpiresAt       *time.Time
	RotatedAt       *time.Time
	UpdatedAt       time.Time
}

// IntegrationInput is the body of adminPutIntegration: the account and the
// tokens (write-only).
type IntegrationInput struct {
	ExternalAccount string
	AccessToken     string
	RefreshToken    string
	ExpiresAt       *time.Time
}

func integrationOf(c Credential) Integration {
	out := Integration{Provider: c.Provider, HasAccessToken: len(c.AccessTokenEnc) > 0,
		HasRefreshToken: len(c.RefreshTokenEnc) > 0, ExpiresAt: c.ExpiresAt, RotatedAt: c.RotatedAt, UpdatedAt: c.UpdatedAt}
	if c.ExternalAccount != nil {
		out.ExternalAccount = *c.ExternalAccount
	}
	return out
}

// AdminListIntegrations implements Service.
func (s *service) AdminListIntegrations(ctx context.Context, a authz.Actor) ([]Integration, error) {
	if err := authz.Can(ctx, a, authz.BillingIntegrationManage, adminSurface); err != nil {
		return nil, err
	}
	rows, err := s.d.Store.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Integration, len(rows))
	for i, c := range rows {
		out[i] = integrationOf(c)
	}
	return out, nil
}

// AdminPutIntegration implements Service (03 §6.2 R): authz (403) → the
// provider and the input (422) → the re-authentication (403
// identity.reauth_*) → the tokens sealed with the shared envelope (no
// envelope: 503, nothing is stored in clear) → the row replaced → audit
// (the provider and which tokens changed; never a value).
func (s *service) AdminPutIntegration(ctx context.Context, a authz.Actor, provider string, in IntegrationInput, proof identity.ReauthProof) (Integration, error) {
	if err := authz.Can(ctx, a, authz.BillingIntegrationManage, adminSurface); err != nil {
		return Integration{}, err
	}
	account := strings.TrimSpace(in.ExternalAccount)
	switch {
	case !slices.Contains(integrationProviders, provider):
		return Integration{}, problem.Field("path.provider", "must be parasut, iyzico, iys or email")
	case utf8.RuneCountInString(account) > maxExternalAccount:
		return Integration{}, problem.Field("body.external_account", "must be at most 100 characters")
	case in.AccessToken == "" && in.RefreshToken == "":
		return Integration{}, problem.Field("body.credentials", "needs an access_token or a refresh_token")
	case len(in.AccessToken) > maxTokenBytes || len(in.RefreshToken) > maxTokenBytes:
		return Integration{}, problem.Field("body.credentials", "a token must be at most 8 KiB")
	case in.ExpiresAt != nil && !in.ExpiresAt.After(s.d.Clock.Now()):
		return Integration{}, problem.Field("body.credentials.expires_at", "must be in the future")
	}
	if s.d.Reauth == nil {
		return Integration{}, problem.ErrUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		// A refused proof is audited as denied; overload writes nothing (03 §6.2, BE-074).
		denied := audit.Event{Action: string(authz.BillingIntegrationManage), ResourceType: "integration",
			Metadata: map[string]any{"provider": provider}}.By(a)
		return Integration{}, audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied, err)
	}
	if s.d.Sealer == nil {
		return Integration{}, problem.ErrUnavailable
	}
	c := Credential{Provider: provider}
	if account != "" {
		c.ExternalAccount = &account
	}
	if in.AccessToken != "" {
		if c.AccessTokenEnc, err = s.d.Sealer.Seal(ctx, AccessTokenPurposePrefix+provider, []byte(in.AccessToken)); err != nil {
			return Integration{}, err
		}
	}
	if in.RefreshToken != "" {
		if c.RefreshTokenEnc, err = s.d.Sealer.Seal(ctx, RefreshTokenPurposePrefix+provider, []byte(in.RefreshToken)); err != nil {
			return Integration{}, err
		}
	}
	now := s.now()
	c.RotatedAt = &now
	if in.ExpiresAt != nil {
		exp := stamp(*in.ExpiresAt)
		c.ExpiresAt = &exp
	}
	var stored Credential
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		if stored, err = s.d.Store.PutCredential(ctx, c); err != nil {
			return err
		}
		// Which tokens were replaced, never a value (audit refuses credential-named keys).
		replaced := []any{}
		if in.AccessToken != "" {
			replaced = append(replaced, "access")
		}
		if in.RefreshToken != "" {
			replaced = append(replaced, "refresh")
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.BillingIntegrationManage), Outcome: audit.Success, ResourceType: "integration",
			Metadata: map[string]any{"provider": provider, "replaced": replaced, audit.MetaReauth: kind},
		}.By(a))
	})
	if err != nil {
		return Integration{}, err
	}
	return integrationOf(stored), nil
}
