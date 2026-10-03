//go:build integration

package identity_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/ratelimit"
	"koro/internal/platform/testutil"
	"koro/internal/platform/token"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) storeWorld { return storeWorld{s: identity.NewGormStore(testutil.NewDB(t).ORM)} })
}

// failingOutbox fails identity's own events (after the consent step of a
// registration) to prove the rollback.
type failingOutbox struct{ outbox.Writer }

var errOutboxDown = errors.New("outbox down")

func (failingOutbox) Add(context.Context, domainevent.Envelope) error { return errOutboxDown }

type passReauth struct{}

func (passReauth) Verify(context.Context, uuid.UUID, consent.ReauthProof) (string, error) {
	return "password", nil
}

// pgIdentity is identity over PostgreSQL with the real consent and audit
// modules.
type pgIdentity struct {
	clk     *clock.Fake
	d       *db.DB
	consent consent.Service
	privacy consent.Notice
	cc      consent.Notice
	build   func(t *testing.T, out outbox.Writer) identity.Service
}

func newPGIdentity(t *testing.T) *pgIdentity {
	t.Helper()
	d := testutil.NewDB(t)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	log := obs.Discard()
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: outbox.NewRegistry(), Queue: queue.NewMem(), Clock: clk, Log: log})
	au := audit.New(audit.Deps{Store: audit.NewGormStore(d.ORM), Tx: d.Tx, Clock: clk, Log: log}).Service
	idem := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: log})
	cm, err := consent.New(consent.Deps{Store: consent.NewGormStore(d.ORM), Tx: d.Tx, Outbox: ob, Audit: au, Idempotency: idem,
		Clock: clk, Log: log, Reauth: passReauth{}})
	if err != nil {
		t.Fatal(err)
	}
	p := &pgIdentity{d: d, consent: cm.Service, clk: clk}
	dpo := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), PlatformRoles: []authz.PlatformRole{authz.PlatformDPO}, MFAVerified: true}
	// notice_versions.created_by → users: the dpo needs an account.
	if _, err := d.Pool.Exec(t.Context(), `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
		VALUES ($1, 'active', $2, $2, 'Dpo', 'Koro', 'platform')`, dpo.UserID, dpo.UserID.String()+"@dpo.test"); err != nil {
		t.Fatal(err)
	}
	publish := func(kind consent.NoticeKind, purpose *consent.Purpose, aud consent.Audience) consent.Notice {
		n, err := cm.Service.CreateNotice(t.Context(), dpo, consent.NoticeDraft{Kind: kind, Purpose: purpose, Audience: aud,
			Locale: consent.LocaleTR, Version: "2026.10.01", Title: "Metin", BodyMD: "Metin " + string(kind) + "\n"}, consent.ReauthProof{Password: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if n, err = cm.Service.PublishNotice(t.Context(), dpo, n.ID, consent.ReauthProof{Password: "x"}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	cc := consent.PurposeCommercialEmail
	p.privacy = publish(consent.KindPrivacyNotice, nil, consent.AudienceStudent)
	p.cc = publish(consent.KindConsentText, &cc, consent.AudienceAll)
	der, err := crypto.NewDeriver([]crypto.Key{{ID: "k1", Secret: []byte("identity-integration-token-key-identity-integ")}})
	if err != nil {
		t.Fatal(err)
	}
	p.build = func(t *testing.T, out outbox.Writer) identity.Service {
		t.Helper()
		if out == nil {
			out = ob
		}
		m, err := identity.New(identity.Deps{
			Store: identity.NewGormStore(d.ORM), Tx: d.Tx, Outbox: out, Queue: queue.NewMem(), Audit: au, Clock: clk, Log: log,
			Limiter: ratelimit.NewMem(clk), Deriver: der, Consent: cm.Service, Notices: lookup{cm.Service}, Mailer: &mailer{},
			LinkBaseURL: "https://app.koro.test", Hasher: sharedHasher(), Secrets: testEnvelope(), Challenges: identity.NewMemChallenges(clk),
		})
		if err != nil {
			t.Fatal(err)
		}
		return m.Service
	}
	return p
}

// lookup is the wire adapter of cmd/koro (noticeLookup) for this test.
type lookup struct{ svc consent.Service }

func (l lookup) NoticeByID(ctx context.Context, id uuid.UUID) (identity.NoticeInfo, error) {
	n, err := l.svc.GetNotice(ctx, authz.Anonymous(), id)
	if err != nil {
		return identity.NoticeInfo{}, err
	}
	info := identity.NoticeInfo{ID: n.ID, Kind: n.Kind, Audience: n.Audience, BodySHA256: n.BodySHA256, Published: n.Status() == consent.StatusPublished}
	if n.PurposeCode != nil {
		info.Purpose = *n.PurposeCode
	}
	return info, nil
}

func (p *pgIdentity) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := p.d.Pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (p *pgIdentity) registration(email string) identity.Registration {
	b, _ := civil.New(2002, time.May, 5)
	return identity.Registration{
		Email: email, Password: goodPassword, FirstName: "Ada", LastName: "Yılmaz", BirthDate: &b, Persona: identity.PersonaStudent,
		Locale: identity.LocaleTR, PrivacyNoticeID: p.privacy.ID, Client: identity.ClientWeb,
		Consents: []identity.ConsentGrant{{Purpose: consent.PurposeCommercialEmail, NoticeVersionID: p.cc.ID, TextSHA256: p.cc.BodySHA256}},
	}
}

// TestRegister_Postgres_UserAndConsentCommitTogether: the account, its
// consent (+ İYS submission), notice.shown and the token commit in one
// transaction, and roll back together (01 §6.3).
func TestRegister_Postgres_UserAndConsentCommitTogether(t *testing.T) {
	t.Parallel()
	p := newPGIdentity(t)
	svc := p.build(t, nil)
	if err := svc.Register(t.Context(), authz.Anonymous(), p.registration("pg@example.com")); err != nil {
		t.Fatal(err)
	}
	id, ok, err := svc.FindByEmail(t.Context(), "pg@example.com")
	if err != nil || !ok {
		t.Fatalf("account missing: %v", err)
	}
	if n := p.count(t, `SELECT count(*) FROM consents WHERE user_id = $1 AND purpose_code = 'commercial_email' AND action = 'granted'`, id); n != 1 {
		t.Fatalf("consents = %d", n)
	}
	if n := p.count(t, `SELECT count(*) FROM audit_events WHERE action = 'notice.shown' AND resource_id = $1`, p.privacy.ID); n != 1 {
		t.Fatalf("notice.shown audit rows = %d", n)
	}
	if n := p.count(t, `SELECT count(*) FROM email_tokens WHERE user_id = $1 AND purpose = 'account_email'`, id); n != 1 {
		t.Fatalf("tokens = %d", n)
	}

	// The token event fails after the consent step: nothing remains.
	failing := p.build(t, failingOutbox{})
	err = failing.Register(t.Context(), authz.Anonymous(), p.registration("rollback@example.com"))
	if !errors.Is(err, errOutboxDown) {
		t.Fatalf("Register = %v, want the outbox failure", err)
	}
	if n := p.count(t, `SELECT count(*) FROM users WHERE email_normalized = 'rollback@example.com'`); n != 0 {
		t.Fatal("the account survived the rollback")
	}
	if n := p.count(t, `SELECT count(*) FROM consents c JOIN users u ON u.id = c.user_id WHERE u.email_normalized = 'rollback@example.com'`); n != 0 {
		t.Fatal("the consent survived the rollback")
	}
	if n := p.count(t, `SELECT count(*) FROM consents`); n != 1 {
		t.Fatalf("consents after the failed registration = %d, want 1", n)
	}
}

// TestBootstrapAdmin_Postgres: the bootstrap rows pass the 02 §5.7 CHECKs
// and a re-run changes nothing; the roles are in effect (06 H-33).
func TestBootstrapAdmin_Postgres(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	store := identity.NewGormStore(d.ORM)
	au := audit.New(audit.Deps{Store: audit.NewGormStore(d.ORM), Tx: d.Tx, Clock: clk, Log: obs.Discard()}).Service
	b, err := identity.NewBootstrapper(identity.BootstrapDeps{Store: store, Tx: d.Tx, Audit: au, Clock: clk, Log: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	admins := []identity.AdminSeed{{Email: "a@koro.test", FirstName: "Ada", LastName: "Yılmaz"}, {Email: "b@koro.test", FirstName: "Can", LastName: "Demir"}}
	out, err := b.Run(t.Context(), admins)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(t.Context(), admins); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var rows int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM platform_role_assignments WHERE reason = 'bootstrap' AND requested_by IS NULL AND status = 'active'`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("bootstrap rows = %d, %v", rows, err)
	}
	for _, o := range out {
		roles, err := store.PlatformRoles(t.Context(), o.UserID)
		if err != nil || len(roles) != 1 || roles[0] != authz.PlatformAdmin {
			t.Fatalf("roles of %s = %v, %v", o.UserID, roles, err)
		}
	}
	if _, err := b.Run(t.Context(), []identity.AdminSeed{admins[0], {Email: "c@koro.test", FirstName: "Eve", LastName: "Kara"}}); !errors.Is(err, identity.ErrBootstrap) {
		t.Fatalf("third admin = %v", err)
	}
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE email_normalized = 'c@koro.test'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused bootstrap left %d accounts", rows)
	}
}

// pgUser inserts an active account with goodPassword and the given roles
// in effect (bootstrap rows) and returns its signed-in actor.
func (p *pgIdentity) pgUser(t *testing.T, store identity.Store, email string, roles ...authz.PlatformRole) authz.Actor {
	t.Helper()
	hash, err := sharedHasher().Hash(t.Context(), goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	now := p.clk.Now().UTC().Truncate(time.Microsecond)
	u := identity.User{ID: uuid.NewV7(), Status: identity.StatusActive, Email: email, EmailNormalized: email, EmailVerifiedAt: &now,
		PasswordHash: hash, FirstName: "Pg", LastName: "User", Persona: identity.PersonaPlatform, Locale: identity.LocaleTR,
		CreatedAt: now, UpdatedAt: now}
	if err := store.InsertUser(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		ra := identity.RoleAssignment{ID: uuid.NewV7(), UserID: u.ID, Role: r, Reason: "bootstrap", Status: identity.RoleActive,
			RequestedAt: now, GrantedAt: &now}
		if err := store.InsertRoleAssignment(t.Context(), ra); err != nil {
			t.Fatal(err)
		}
	}
	return authz.Actor{Kind: authz.KindUser, UserID: u.ID, SessionID: uuid.NewV7(), PlatformRoles: roles, MFAVerified: true}
}

// TestRoles_FourEyesOnPostgres runs the 06 H-17 flow over PostgreSQL:
// serializable transactions, the CHECKs and the 06 H-33 reader.
func TestRoles_FourEyesOnPostgres(t *testing.T) {
	t.Parallel()
	p := newPGIdentity(t)
	svc := p.build(t, nil)
	store := identity.NewGormStore(p.d.ORM)
	ctx := t.Context()
	a := p.pgUser(t, store, "pg-a@koro.test", authz.PlatformAdmin)
	b := p.pgUser(t, store, "pg-b@koro.test", authz.PlatformAdmin)
	target := p.pgUser(t, store, "pg-target@koro.test")
	pw := identity.ReauthProof{Password: goodPassword}

	ra, err := svc.GrantPlatformRole(ctx, a, identity.RoleGrant{UserID: target.UserID, Role: authz.PlatformFinanceAdmin, Reason: "finance desk", Proof: pw})
	if err != nil || ra.Status != identity.RolePending {
		t.Fatalf("request %+v %v", ra, err)
	}
	if roles, _ := store.PlatformRoles(ctx, target.UserID); len(roles) != 0 {
		t.Fatalf("pending request in effect: %v", roles)
	}
	if _, err := svc.ApprovePlatformRole(ctx, a, ra.ID, pw); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("self approval = %v", err)
	}
	if _, err := svc.ApprovePlatformRole(ctx, b, ra.ID, pw); err != nil {
		t.Fatalf("approval: %v", err)
	}
	if n := p.count(t, `SELECT count(*) FROM platform_role_assignments WHERE id = $1 AND status = 'active' AND approved_by = $2 AND requested_by = $3`,
		ra.ID, b.UserID, a.UserID); n != 1 {
		t.Fatal("approved row not as expected")
	}
	if _, err := svc.RevokePlatformRole(ctx, a, ra.ID, "left finance", pw); err != nil {
		t.Fatalf("revoke request: %v", err)
	}
	if _, err := svc.RevokePlatformRole(ctx, a, ra.ID, "confirm myself", pw); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("self confirmation = %v", err)
	}
	if _, err := svc.RevokePlatformRole(ctx, b, ra.ID, "confirmed", pw); err != nil {
		t.Fatalf("confirmation: %v", err)
	}
	if roles, _ := store.PlatformRoles(ctx, target.UserID); len(roles) != 0 {
		t.Fatalf("revoked role in effect: %v", roles)
	}
	// The DB CHECK is the second layer: a direct write of a self-approval fails.
	if _, err := p.d.Pool.Exec(ctx, `INSERT INTO platform_role_assignments (user_id, role, reason, status, requested_by, approved_by, approved_at, granted_at)
		VALUES ($1, 'talent_auditor', 'single eye', 'active', $2, $2, now(), now())`, target.UserID, a.UserID); err == nil {
		t.Fatal("approved_by = requested_by accepted by the database")
	}
	// The last-admin rule over PostgreSQL.
	bRows, _ := store.RoleAssignmentsOf(ctx, b.UserID)
	aRows, _ := store.RoleAssignmentsOf(ctx, a.UserID)
	if _, err := svc.RevokePlatformRole(ctx, a, bRows[0].ID, "b leaves", pw); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RevokePlatformRole(ctx, b, aRows[0].ID, "a leaves", pw); !errors.Is(err, identity.ErrLastPlatformAdmin) {
		t.Fatalf("last admin = %v", err)
	}
}

// TestMFA_OnPostgres: enrollment, sealed storage, replay guard and admin
// search over PostgreSQL.
func TestMFA_OnPostgres(t *testing.T) {
	t.Parallel()
	p := newPGIdentity(t)
	svc := p.build(t, nil)
	store := identity.NewGormStore(p.d.ORM)
	ctx := t.Context()
	u := p.pgUser(t, store, "pg-mfa@example.com")
	if _, err := p.d.Pool.Exec(ctx, `INSERT INTO sessions (id, user_id, token_hash, client_kind, idle_expires_at, absolute_expires_at)
		VALUES ($1, $2, $3, 'web', now() + interval '1 hour', now() + interval '2 hours')`, u.SessionID, u.UserID, token.Hash(u.SessionID.String())); err != nil {
		t.Fatal(err)
	}
	enr, err := svc.EnrollTOTP(ctx, u, identity.ReauthProof{Password: goodPassword})
	if err != nil {
		t.Fatal(err)
	}
	if n := p.count(t, `SELECT count(*) FROM mfa_totp WHERE user_id = $1 AND position(convert_to($2, 'UTF8') in secret_enc) = 0`, u.UserID, enr.Secret); n != 1 {
		t.Fatal("the TOTP secret is stored in clear")
	}
	if _, err := svc.ConfirmTOTP(ctx, u, totpCode(t, enr.Secret, p.clk.Now()), identity.ReauthProof{Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	if n := p.count(t, `SELECT count(*) FROM mfa_recovery_codes WHERE user_id = $1 AND used_at IS NULL`, u.UserID); n != 10 {
		t.Fatalf("%d recovery codes", n)
	}
	if n := p.count(t, `SELECT count(*) FROM sessions WHERE id = $1 AND mfa_verified_at IS NOT NULL`, u.SessionID); n != 1 {
		t.Fatal("session not MFA-verified")
	}
	p.clk.Advance(30 * time.Second)
	code := totpCode(t, enr.Secret, p.clk.Now())
	if err := svc.VerifyMFA(ctx, u, code); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := svc.VerifyMFA(ctx, u, code); !errors.Is(err, identity.ErrInvalidMFACode) {
		t.Fatalf("replay = %v", err)
	}
	sup := p.pgUser(t, store, "pg-sup@koro.test", authz.PlatformSupport)
	res, err := svc.SearchUsers(ctx, sup, identity.AdminUserQuery{Q: "pg-mfa@example.com"})
	if err != nil || len(res.Items) != 1 || !res.Items[0].MFAEnabled {
		t.Fatalf("search %+v %v", res, err)
	}
	if res, err = svc.SearchUsers(ctx, sup, identity.AdminUserQuery{Q: "pg us"}); err != nil || len(res.Items) < 2 {
		t.Fatalf("name search %d %v", len(res.Items), err)
	}
}

// TestAccountActions_FourEyesOnPostgres is 06 H-47 over PostgreSQL: the
// suspension of a privileged holder waits for a second platform_admin, and
// the CHECKs of privileged_account_actions hold.
func TestAccountActions_FourEyesOnPostgres(t *testing.T) {
	t.Parallel()
	p := newPGIdentity(t)
	svc := p.build(t, nil)
	store := identity.NewGormStore(p.d.ORM)
	ctx := t.Context()
	a := p.pgUser(t, store, "pg-h47-a@koro.test", authz.PlatformAdmin)
	b := p.pgUser(t, store, "pg-h47-b@koro.test", authz.PlatformAdmin)
	target := p.pgUser(t, store, "pg-h47-t@koro.test", authz.PlatformAdmin)

	res, err := svc.SuspendUser(ctx, a, target.UserID, "suspicious grants")
	if err != nil || res.Pending == nil {
		t.Fatalf("first eye: %+v %v", res.Pending, err)
	}
	if _, err := svc.SuspendUser(ctx, a, target.UserID, "suspicious grants"); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("requester again = %v", err)
	}
	if res, err = svc.SuspendUser(ctx, b, target.UserID, "approved by b"); err != nil || res.Pending != nil || res.User.User.Status != identity.StatusSuspended {
		t.Fatalf("second eye: %+v %v", res, err)
	}
	if n := p.count(t, `SELECT count(*) FROM privileged_account_actions WHERE target_user_id = $1 AND status = 'approved'
		AND requested_by = $2 AND approved_by = $3`, target.UserID, a.UserID, b.UserID); n != 1 {
		t.Fatal("approved request row not as expected")
	}
	for _, bad := range []string{
		`INSERT INTO privileged_account_actions (target_user_id, action, reason, requested_by, expires_at, status, approved_by, approved_at)
			VALUES ($1, 'reset_mfa', 'single eye', $2, now() + interval '1 day', 'approved', $2, now())`,
		`INSERT INTO privileged_account_actions (target_user_id, action, reason, requested_by, expires_at, status)
			VALUES ($1, 'reset_mfa', 'approved without approver', $2, now() + interval '1 day', 'approved')`,
		`INSERT INTO privileged_account_actions (target_user_id, action, reason, requested_by, expires_at)
			VALUES ($1, 'reset_mfa', 'self request', $1, now() + interval '1 day')`,
	} {
		if _, err := p.d.Pool.Exec(ctx, bad, target.UserID, a.UserID); err == nil {
			t.Fatalf("accepted by the database: %s", bad)
		}
	}
}
