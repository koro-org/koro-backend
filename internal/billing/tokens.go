package billing

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
)

// Token rotation of a provider integration (BE-053; 02 §22.5, 01 §7
// billing.refresh_token). The OAuth tokens live sealed in
// integration_credentials (adminPutIntegration enters the first pair; the
// account column is the provider's company ID). Paraşüt's access token
// lasts two hours and every refresh may return a new refresh token that
// replaces the old one, so:
//
//   - one refresher at a time: the row is locked (SELECT … FOR UPDATE) for
//     the refresh, and whoever waited for the lock reads the renewed row
//     instead of refreshing again. The refresh call is the one external
//     call billing makes inside a transaction (02 §22.5; bounded by the
//     refresher's timeout);
//   - the job renews a token that expires within refreshAhead every 30
//     minutes, a call renews an expired token before use, and a 401
//     renews the token it was refused with (Refresh) once;
//   - tokens never reach a log line, an error or the audit record (which
//     names the provider and whether the refresh token rotated).
const (
	// RefreshTokenTaskName is the scheduled rotation (every 30 minutes).
	RefreshTokenTaskName = "billing.refresh_token"
	// RefreshTokenCron is its schedule.
	RefreshTokenCron = "@every 30m"

	refreshAhead     = time.Hour     // the job renews a token expiring within this
	expiryMargin     = time.Minute   // a token this close to its expiry is renewed before use
	defaultTokenLife = 2 * time.Hour // when the provider gives no lifetime (Paraşüt: 7200 s)

	auditIntegrationRefresh = "billing.integration.refresh"
)

// Errors of the token rotation.
var (
	// ErrNoCredentials: the integration has no credentials row or no usable
	// access token; an administrator enters them (adminPutIntegration).
	ErrNoCredentials = errors.New("billing: the provider integration has no credentials")
	// ErrRefreshRefused: the provider refused the refresh token (or there is
	// none); an administrator enters new tokens.
	ErrRefreshRefused = errors.New("billing: the provider refused the refresh token")
)

// TokenGrant is a provider's answer to a refresh.
type TokenGrant struct {
	AccessToken  string
	RefreshToken string        // "" keeps the current refresh token
	ExpiresIn    time.Duration // 0 → two hours
}

// String keeps the tokens out of %v.
func (TokenGrant) String() string { return "billing.TokenGrant{redacted}" }

// GoString keeps the tokens out of %#v.
func (g TokenGrant) GoString() string { return g.String() }

// LogValue keeps the tokens out of slog.
func (g TokenGrant) LogValue() slog.Value { return slog.StringValue(g.String()) }

// ProviderToken is what a provider call needs: the access token and the
// account it acts for (integration_credentials.external_account: Paraşüt's
// company ID).
type ProviderToken struct {
	Access  string
	Account string
}

// String keeps the token out of %v.
func (t ProviderToken) String() string { return "billing.ProviderToken{account:" + t.Account + "}" }

// GoString keeps the token out of %#v.
func (t ProviderToken) GoString() string { return t.String() }

// LogValue keeps the token out of slog.
func (t ProviderToken) LogValue() slog.Value { return slog.StringValue(t.String()) }

// TokenDeps are the dependencies of ProviderTokens; all are required.
type TokenDeps struct {
	Provider  string // integration_credentials.provider ("parasut")
	Store     Store
	Tx        db.TxManager
	Sealer    Sealer
	Refresher TokenRefresher
	Clock     clock.Clock
	Log       *slog.Logger
	Audit     AuditLogger
}

// ProviderTokens is the token source of a provider adapter over
// integration_credentials (ParasutProvider's Tokens) and the rotation job.
type ProviderTokens struct{ d TokenDeps }

// NewProviderTokens checks d.
func NewProviderTokens(d TokenDeps) (*ProviderTokens, error) {
	switch {
	case d.Provider == "" || d.Store == nil || d.Tx == nil || d.Sealer == nil || d.Refresher == nil || d.Clock == nil ||
		d.Log == nil || d.Audit == nil:
		return nil, errors.New("billing: TokenDeps needs Provider, Store, Tx, Sealer, Refresher, Clock, Log and Audit")
	case !slices.Contains(integrationProviders, d.Provider):
		return nil, fmt.Errorf("billing: TokenDeps.Provider %q is not an integration provider", d.Provider)
	}
	return &ProviderTokens{d: d}, nil
}

// Token returns the access token to call the provider with, renewing it
// first when it has expired (or is about to).
func (t *ProviderTokens) Token(ctx context.Context) (ProviderToken, error) {
	c, err := t.credential(ctx, false)
	if err != nil {
		return ProviderToken{}, err
	}
	tok, err := t.open(ctx, c)
	if err != nil {
		return ProviderToken{}, err
	}
	if tok.Access != "" && !t.expiring(c, expiryMargin) {
		return tok, nil
	}
	return t.rotate(ctx, func(cur Credential, access string) bool { return access != "" && !t.expiring(cur, expiryMargin) })
}

// Refresh renews the token the provider refused (stale, a 401) unless
// another worker renewed it meanwhile; then it returns the current one.
func (t *ProviderTokens) Refresh(ctx context.Context, stale string) (ProviderToken, error) {
	return t.rotate(ctx, func(_ Credential, access string) bool {
		return access != "" && subtle.ConstantTimeCompare([]byte(access), []byte(stale)) == 0
	})
}

// RefreshDue is the billing.refresh_token job: it renews the token when it
// expires within an hour (or its expiry is unknown). No credentials or no
// refresh token: nothing to do.
func (t *ProviderTokens) RefreshDue(ctx context.Context) error {
	c, err := t.credential(ctx, false)
	switch {
	case errors.Is(err, ErrNoCredentials):
		t.d.Log.LogAttrs(ctx, slog.LevelDebug, "integration token rotation skipped: no credentials",
			slog.String("provider", t.d.Provider))
		return nil
	case err != nil:
		return err
	case len(c.RefreshTokenEnc) == 0:
		t.d.Log.LogAttrs(ctx, slog.LevelDebug, "integration token rotation skipped: no refresh token",
			slog.String("provider", t.d.Provider))
		return nil
	case !t.expiring(c, refreshAhead):
		return nil
	}
	_, err = t.rotate(ctx, func(cur Credential, _ string) bool { return !t.expiring(cur, refreshAhead) })
	return err
}

// expiring reports whether c's access token expires within d (an unknown
// expiry counts as expiring).
func (t *ProviderTokens) expiring(c Credential, d time.Duration) bool {
	return c.ExpiresAt == nil || !t.d.Clock.Now().Before(c.ExpiresAt.Add(-d))
}

func (t *ProviderTokens) credential(ctx context.Context, lock bool) (Credential, error) {
	c, err := t.d.Store.Credential(ctx, t.d.Provider, lock)
	if errors.Is(err, ErrNotFound) {
		return Credential{}, fmt.Errorf("%w (%s)", ErrNoCredentials, t.d.Provider)
	}
	return c, err
}

// open returns the access token and the account of c ("" when c has no
// access token).
func (t *ProviderTokens) open(ctx context.Context, c Credential) (ProviderToken, error) {
	tok := ProviderToken{}
	if c.ExternalAccount != nil {
		tok.Account = *c.ExternalAccount
	}
	if len(c.AccessTokenEnc) == 0 {
		return tok, nil
	}
	raw, err := t.d.Sealer.Open(ctx, AccessTokenPurposePrefix+t.d.Provider, c.AccessTokenEnc)
	if err != nil {
		return ProviderToken{}, fmt.Errorf("billing: open the %s access token: %w", t.d.Provider, err)
	}
	tok.Access = string(raw)
	clear(raw)
	return tok, nil
}

// rotate renews the token under the row lock unless keep (given the locked
// row and its access token) says the current one stays.
func (t *ProviderTokens) rotate(ctx context.Context, keep func(Credential, string) bool) (ProviderToken, error) {
	var (
		out     ProviderToken
		rotated bool
	)
	err := t.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		c, err := t.credential(ctx, true) // FOR UPDATE: one refresher (02 §22.5)
		if err != nil {
			return err
		}
		cur, err := t.open(ctx, c)
		if err != nil {
			return err
		}
		if keep(c, cur.Access) {
			out = cur
			return nil
		}
		if len(c.RefreshTokenEnc) == 0 {
			return fmt.Errorf("%w: %s has no refresh token", ErrRefreshRefused, t.d.Provider)
		}
		refresh, err := t.d.Sealer.Open(ctx, RefreshTokenPurposePrefix+t.d.Provider, c.RefreshTokenEnc)
		if err != nil {
			return fmt.Errorf("billing: open the %s refresh token: %w", t.d.Provider, err)
		}
		g, err := t.d.Refresher.Refresh(ctx, string(refresh))
		clear(refresh)
		if err != nil {
			return err
		}
		if g.AccessToken == "" {
			return fmt.Errorf("billing: the %s refresh returned no access token", t.d.Provider)
		}
		next := c
		if next.AccessTokenEnc, err = t.d.Sealer.Seal(ctx, AccessTokenPurposePrefix+t.d.Provider, []byte(g.AccessToken)); err != nil {
			return err
		}
		if g.RefreshToken != "" {
			if next.RefreshTokenEnc, err = t.d.Sealer.Seal(ctx, RefreshTokenPurposePrefix+t.d.Provider, []byte(g.RefreshToken)); err != nil {
				return err
			}
		}
		life := g.ExpiresIn
		if life <= 0 {
			life = defaultTokenLife
		}
		now := t.d.Clock.Now()
		exp, at := stamp(now.Add(life)), stamp(now)
		next.ExpiresAt, next.RotatedAt = &exp, &at
		if _, err := t.d.Store.PutCredential(ctx, next); err != nil {
			return err
		}
		out, rotated = ProviderToken{Access: g.AccessToken, Account: cur.Account}, true
		return t.d.Audit.Log(ctx, audit.Event{
			Action: auditIntegrationRefresh, Outcome: audit.Success, ResourceType: "integration",
			Metadata: map[string]any{"provider": t.d.Provider, "rotated": g.RefreshToken != ""},
		}.By(authz.SystemActor(RefreshTokenTaskName)))
	})
	if err != nil {
		level, attrs := slog.LevelWarn, []slog.Attr{slog.String("provider", t.d.Provider), slog.String("err", err.Error())}
		if errors.Is(err, ErrRefreshRefused) || errors.Is(err, ErrNoCredentials) {
			level, attrs = slog.LevelError, append(attrs, slog.Bool("alarm", true))
		}
		t.d.Log.LogAttrs(ctx, level, "integration token not renewed", attrs...)
		return ProviderToken{}, err
	}
	if rotated {
		t.d.Log.LogAttrs(ctx, slog.LevelInfo, "integration token renewed", slog.String("provider", t.d.Provider))
	}
	return out, nil
}
