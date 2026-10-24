//go:build integration

package identity_test

import (
	"context"
	"encoding/base32"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/crypto"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/ratelimit"
)

// failTopic fails one topic of the outbox (after the rows before it were
// written) to prove the rollback of the whole confirmation.
type failTopic struct {
	outbox.Writer
	topic string
}

func (f failTopic) Add(ctx context.Context, env domainevent.Envelope) error {
	if env.Topic == f.topic {
		return errOutboxDown
	}
	return f.Writer.Add(ctx, env)
}

// pgSignup is identity over PostgreSQL with a directory that knows one
// university (uni.edu.tr and its subdomains).
type pgSignup struct {
	*pgIdentity
	der   *crypto.Deriver
	uni   uuid.UUID
	ob    outbox.Writer
	build func(out outbox.Writer) identity.Service
	store func(identity.Store) identity.Store // nil: the GORM store as is
}

// racyStore misses the holder of an address in the pre-check, as if the
// other account verified it between the check and the insert.
type racyStore struct{ identity.Store }

func (racyStore) OpenEduVerificationOfEmail(context.Context, string) (identity.EduRecord, bool, error) {
	return identity.EduRecord{}, false, nil
}

func newPGSignup(t *testing.T) *pgSignup {
	t.Helper()
	p := newPGIdentity(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := p.d.Pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	org, uni := uuid.NewV7(), uuid.NewV7()
	exec(`INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'university', $2, $3)`, org, "Üni "+org.String(), "uni-"+org.String())
	exec(`INSERT INTO universities (id, org_id, yok_code, name, city, type) VALUES ($1, $2, $3, $4, 'Ankara', 'state')`,
		uni, org, "Y"+uni.String()[:8], "Üniversite "+uni.String())
	der, err := crypto.NewDeriver([]crypto.Key{{ID: "k1", Secret: []byte("identity-integration-signup-key-identity-signup")}})
	if err != nil {
		t.Fatal(err)
	}
	log := obs.Discard()
	s := &pgSignup{pgIdentity: p, der: der, uni: uni,
		ob: outbox.New(outbox.Deps{Store: outbox.NewGormStore(p.d.ORM), Tx: p.d.Tx, Subs: outbox.NewRegistry(), Queue: queue.NewMem(),
			Clock: p.clk, Log: log})}
	au := audit.New(audit.Deps{Store: audit.NewGormStore(p.d.ORM), Tx: p.d.Tx, Clock: p.clk, Log: log}).Service
	s.build = func(out outbox.Writer) identity.Service {
		store := identity.NewGormStore(p.d.ORM)
		if s.store != nil {
			store = s.store(store)
		}
		m, err := identity.New(identity.Deps{
			Store: store, Tx: p.d.Tx, Outbox: out, Queue: queue.NewMem(), Audit: au, Clock: p.clk, Log: log,
			Limiter: ratelimit.NewMem(p.clk), Deriver: der, Consent: p.consent, Notices: lookup{p.consent}, Mailer: &mailer{},
			LinkBaseURL: "https://app.koro.test", Hasher: sharedHasher(), Secrets: testEnvelope(), Challenges: identity.NewMemChallenges(p.clk),
			Domains: func(_ context.Context, domain string) (uuid.UUID, bool, error) {
				return uni, domain == "uni.edu.tr" || strings.HasSuffix(domain, ".uni.edu.tr"), nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return m.Service
	}
	return s
}

// token re-derives the open account_email token of userID (the worker's
// e-mail) and returns it with the stored target_email.
func (s *pgSignup) token(t *testing.T, userID uuid.UUID) (raw, target string) {
	t.Helper()
	var id uuid.UUID
	var keyID string
	var tgt *string
	err := s.d.Pool.QueryRow(t.Context(), `SELECT id, key_id, target_email FROM email_tokens
		WHERE user_id = $1 AND purpose = 'account_email' AND consumed_at IS NULL`, userID).Scan(&id, &keyID, &tgt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.der.DerivedToken("identity.email_token", keyID, id[:])
	if err != nil {
		t.Fatal(err)
	}
	if tgt != nil {
		target = *tgt
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), target
}

func (s *pgSignup) register(t *testing.T, svc identity.Service, address string) uuid.UUID {
	t.Helper()
	if err := svc.Register(t.Context(), authz.Anonymous(), s.registration(address)); err != nil {
		t.Fatal(err)
	}
	id, ok, err := svc.FindByEmail(t.Context(), address)
	if err != nil || !ok {
		t.Fatalf("account missing: %v", err)
	}
	return id
}

// TestConfirmEmail_EduSignup_Postgres: the account activation, the student
// verification, user.edu_verified, both audits and the token commit in one
// transaction and roll back together; the rows pass the 02 §5.3/§5.4
// CHECKs; an address verifying another account leaves the student step out
// without aborting the transaction (edu_verifications_email_uq).
func TestConfirmEmail_EduSignup_Postgres(t *testing.T) {
	t.Parallel()
	s := newPGSignup(t)
	ctx := t.Context()
	svc := s.build(s.ob)
	const address = "ada@ogr.uni.edu.tr"
	id := s.register(t, svc, address)
	raw, target := s.token(t, id)
	if target != address {
		t.Fatalf("target_email of the registration token = %q, want the student address", target)
	}

	// user.edu_verified fails after the activation and the row: nothing remains.
	err := s.build(failTopic{Writer: s.ob, topic: domainevent.TopicUserEduVerified}).ConfirmEmail(ctx, authz.Anonymous(), raw)
	if !errors.Is(err, errOutboxDown) {
		t.Fatalf("ConfirmEmail = %v, want the outbox failure", err)
	}
	if n := s.count(t, `SELECT count(*) FROM users WHERE id = $1 AND status = 'pending_email' AND email_verified_at IS NULL`, id); n != 1 {
		t.Fatal("the activation survived the rollback")
	}
	if n := s.count(t, `SELECT count(*) FROM email_tokens WHERE user_id = $1 AND consumed_at IS NULL`, id); n != 1 {
		t.Fatal("the token was consumed by a rolled-back confirmation")
	}
	if n := s.count(t, `SELECT count(*) FROM edu_verifications WHERE user_id = $1`, id); n != 0 {
		t.Fatal("the verification survived the rollback")
	}

	s.clk.Advance(time.Minute)
	if err := svc.ConfirmEmail(ctx, authz.Anonymous(), raw); err != nil {
		t.Fatal(err)
	}
	if n := s.count(t, `SELECT count(*) FROM users WHERE id = $1 AND status = 'active' AND email_verified_at IS NOT NULL`, id); n != 1 {
		t.Fatal("the account is not active")
	}
	if n := s.count(t, `SELECT count(*) FROM edu_verifications WHERE user_id = $1 AND status = 'verified' AND method = 'edu_email'
		AND edu_email_normalized = $2 AND email_domain = 'ogr.uni.edu.tr' AND university_id = $3
		AND expires_at = verified_at + interval '1 year'`, id, address, s.uni); n != 1 {
		t.Fatal("no verified student row")
	}
	if n := s.count(t, `SELECT count(*) FROM outbox o JOIN edu_verifications v ON v.id = o.aggregate_id
		WHERE o.topic = 'user.edu_verified' AND v.user_id = $1 AND (o.payload->>'user_id')::uuid = $1
		AND (o.payload->>'university_id')::uuid = $2 AND (o.payload->>'renewal')::boolean = false
		AND (o.payload->>'expires_at')::timestamptz = v.expires_at`, id, s.uni); n != 1 {
		t.Fatal("user.edu_verified missing or with another payload")
	}
	if n := s.count(t, `SELECT count(*) FROM audit_events WHERE action = 'identity.edu_verification.confirm'
		AND actor_user_id = $1 AND metadata->>'via' = 'account_email'`, id); n != 1 {
		t.Fatal("no student verification audit")
	}
	if n := s.count(t, `SELECT count(*) FROM audit_events WHERE action = 'identity.email.verify' AND resource_id = $1
		AND metadata->>'edu' = 'verified'`, id); n != 1 {
		t.Fatal("no account verification audit with the student result")
	}

	// An address verifying another account: the account alone, in one
	// transaction that commits (no unique violation reaches PostgreSQL).
	holder := s.register(t, svc, "holder@example.com")
	hr, _ := s.token(t, holder)
	if err := svc.ConfirmEmail(ctx, authz.Anonymous(), hr); err != nil {
		t.Fatal(err)
	}
	now := s.clk.Now()
	if _, err := s.d.Pool.Exec(ctx, `INSERT INTO edu_verifications (user_id, method, edu_email, edu_email_normalized, email_domain,
		university_id, status, verified_at, expires_at) VALUES ($1, 'edu_email', $2, $2, 'uni.edu.tr', $3, 'verified', $4, $5)`,
		holder, "taken@uni.edu.tr", s.uni, now, now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	taken := s.register(t, svc, "taken@uni.edu.tr")
	tr, _ := s.token(t, taken)
	if err := svc.ConfirmEmail(ctx, authz.Anonymous(), tr); err != nil {
		t.Fatalf("ConfirmEmail(taken) = %v, want nil", err)
	}
	if n := s.count(t, `SELECT count(*) FROM users WHERE id = $1 AND status = 'active'`, taken); n != 1 {
		t.Fatal("the account was not verified")
	}
	if n := s.count(t, `SELECT count(*) FROM edu_verifications WHERE user_id = $1`, taken); n != 0 {
		t.Fatal("the address verified a second account")
	}
	if n := s.count(t, `SELECT count(*) FROM audit_events WHERE action = 'identity.email.verify' AND resource_id = $1
		AND metadata->>'edu' = 'address_taken'`, taken); n != 1 {
		t.Fatal("the skipped student step is not audited")
	}
	// The race: the pre-check misses the holder, the insert hits
	// edu_verifications_email_uq and ends the transaction; the confirmation
	// runs again without the student step (204, token consumed once).
	raced := s.register(t, svc, "raced@uni.edu.tr")
	other := s.register(t, svc, "holder2@example.com")
	or, _ := s.token(t, other)
	if err := svc.ConfirmEmail(ctx, authz.Anonymous(), or); err != nil {
		t.Fatal(err)
	}
	if _, err := s.d.Pool.Exec(ctx, `INSERT INTO edu_verifications (user_id, method, edu_email, edu_email_normalized, email_domain,
		university_id, status, verified_at, expires_at) VALUES ($1, 'edu_email', $2, $2, 'uni.edu.tr', $3, 'verified', $4, $5)`,
		other, "raced@uni.edu.tr", s.uni, now, now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	s.store = func(st identity.Store) identity.Store { return racyStore{st} }
	racy := s.build(s.ob)
	s.store = nil
	rr, _ := s.token(t, raced)
	if err := racy.ConfirmEmail(ctx, authz.Anonymous(), rr); err != nil {
		t.Fatalf("ConfirmEmail(race) = %v, want nil", err)
	}
	if n := s.count(t, `SELECT count(*) FROM users WHERE id = $1 AND status = 'active'`, raced); n != 1 {
		t.Fatal("the account was not verified after the race")
	}
	if n := s.count(t, `SELECT count(*) FROM edu_verifications WHERE user_id = $1`, raced); n != 0 {
		t.Fatal("the raced address verified a second account")
	}
	if n := s.count(t, `SELECT count(*) FROM audit_events WHERE action = 'identity.email.verify' AND resource_id = $1
		AND metadata->>'edu' = 'address_taken'`, raced); n != 1 {
		t.Fatal("the race is not audited as address_taken")
	}

	// A re-issued token proves the account address only (no target).
	fresh := s.register(t, svc, "yeni@uni.edu.tr")
	if err := svc.ResendVerification(ctx, authz.Anonymous(), "yeni@uni.edu.tr"); err != nil {
		t.Fatal(err)
	}
	if _, tgt := s.token(t, fresh); tgt != "" {
		t.Fatalf("re-issued token target %q", tgt)
	}
}
