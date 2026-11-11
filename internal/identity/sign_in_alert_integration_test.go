//go:build integration

package identity_test

import (
	"errors"
	"testing"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/httpx"
)

// TestNewSignIn_Postgres_AlertCommitsWithTheSession (BE-209): over
// PostgreSQL the new sign-in alert is an outbox row of the sign-in's own
// transaction: a failing outbox rolls the session back (no token, no row,
// last_login_at unchanged), a working one commits both; the account's
// history is read through sessions_user_history_idx, revoked rows included.
func TestNewSignIn_Postgres_AlertCommitsWithTheSession(t *testing.T) {
	t.Parallel()
	p := newPGIdentity(t)
	svc := p.build(t, nil)
	store := identity.NewGormStore(p.d.ORM)
	u := p.pgUser(t, store, "pg-new-sign-in@example.com")
	login := func(svc identity.Service, ip, ua string) (identity.LoginResult, error) {
		ctx := httpx.WithUserAgent(httpx.WithClientIP(t.Context(), ip), ua)
		return svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: "pg-new-sign-in@example.com", Password: goodPassword,
			Client: identity.ClientWeb})
	}
	alerts := func() int {
		return p.count(t, `SELECT count(*) FROM outbox WHERE topic = 'identity.new_sign_in_detected' AND payload->>'user_id' = $1`,
			u.UserID.String())
	}
	home, err := login(svc, "203.0.113.7", uaChrome)
	if err != nil {
		t.Fatal(err)
	}
	if n := alerts(); n != 0 {
		t.Fatalf("the first sign-in wrote %d alerts", n)
	}
	// A revoked session still recognizes its device and network.
	if err := svc.DeleteCurrentSession(t.Context(), authz.Actor{Kind: authz.KindUser, UserID: u.UserID, SessionID: home.Session.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := login(svc, "203.0.113.8", uaChrome); err != nil {
		t.Fatal(err)
	}
	if n := alerts(); n != 0 {
		t.Fatalf("a known device and network wrote %d alerts", n)
	}

	lastLogin := func() string {
		var s string
		if err := p.d.Pool.QueryRow(t.Context(), `SELECT last_login_at::text FROM users WHERE id = $1`, u.UserID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := lastLogin()
	if _, err := login(p.build(t, failingOutbox{}), "192.0.2.10", uaFirefox); !errors.Is(err, errOutboxDown) {
		t.Fatalf("CreateSession = %v, want the outbox failure", err)
	}
	if n := p.count(t, `SELECT count(*) FROM sessions WHERE user_id = $1 AND ip = '192.0.2.10'`, u.UserID); n != 0 {
		t.Fatal("the session survived the failed alert")
	}
	if got := lastLogin(); got != before {
		t.Fatalf("last_login_at %s → %s after the rollback", before, got)
	}

	res, err := login(svc, "192.0.2.10", uaFirefox)
	if err != nil {
		t.Fatal(err)
	}
	if n := p.count(t, `SELECT count(*) FROM outbox WHERE topic = 'identity.new_sign_in_detected' AND aggregate_type = 'session'
		AND aggregate_id = $1 AND payload->>'session_id' = $2`, res.Session.ID, res.Session.ID.String()); n != 1 {
		t.Fatalf("alert rows of the session = %d", n)
	}
	if n := p.count(t, `SELECT count(*) FROM audit_events WHERE action = 'identity.session.create' AND resource_id = $1
		AND metadata->>'new_sign_in' = 'true'`, res.Session.ID); n != 1 {
		t.Fatalf("audit rows marking the alert = %d", n)
	}
}
