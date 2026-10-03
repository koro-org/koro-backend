package identity_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/httpx"
	"koro/internal/platform/token"
)

func TestRegister_ExistingEmail_Returns202NoLeak(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "ada@example.com")
	hashes := e.hasher.hash.Load()

	// Same address (other case), other names: the same nil answer, the
	// same argon2id cost, no second account.
	r := e.registration("ADA@Example.com")
	r.FirstName = "Mallory"
	if err := e.svc.Register(ctx, authz.Anonymous(), r); err != nil {
		t.Fatalf("Register(taken) = %v, want nil (202)", err)
	}
	if got := e.hasher.hash.Load() - hashes; got != 1 {
		t.Fatalf("taken address hashed %d times, want 1 (same cost as a new one)", got)
	}
	if id, _, _ := e.svc.FindByEmail(ctx, "ada@example.com"); id != u.ID {
		t.Fatal("the account changed")
	}
	cur, _ := e.store.User(ctx, u.ID)
	if cur.FirstName != "Ada" || cur.PasswordHash != u.PasswordHash {
		t.Fatal("a taken registration modified the existing account")
	}
	// The owner gets a security notice instead.
	e.runNotices(t)
	last := e.mail.sent()[len(e.mail.sent())-1]
	if last.Template != "identity.security_notice" || last.Data["kind"] != identity.NoticeSignupAttempt || last.ToEmail != "ada@example.com" {
		t.Fatalf("notice = %+v", last)
	}

	// A consent with a wrong digest fails alike for a taken and a new
	// address (validated before the lookup: no oracle).
	bad := e.registration("ada@example.com")
	bad.Consents = []identity.ConsentGrant{{Purpose: consent.PurposeCommercialEmail, NoticeVersionID: e.notices.ccEmail.ID, TextSHA256: make([]byte, 32)}}
	errTaken := e.svc.Register(ctx, authz.Anonymous(), bad)
	bad.Email = "new@example.com"
	errNew := e.svc.Register(ctx, authz.Anonymous(), bad)
	wantErr(t, errTaken, consent.ErrTextMismatch)
	wantErr(t, errNew, consent.ErrTextMismatch)

	// An unverified owner gets a fresh verification e-mail.
	if err := e.svc.Register(ctx, authz.Anonymous(), e.registration("pending@example.com")); err != nil {
		t.Fatal(err)
	}
	first := e.lastToken(t, "identity.verify_email")
	if err := e.svc.Register(ctx, authz.Anonymous(), e.registration("pending@example.com")); err != nil {
		t.Fatal(err)
	}
	second := e.lastToken(t, "identity.verify_email")
	if first == second {
		t.Fatal("no new verification token for an unverified owner")
	}
	wantErr(t, e.svc.ConfirmEmail(ctx, authz.Anonymous(), first), identity.ErrInvalidToken) // replaced
}

func TestRegister_StudentWithoutBirthDate_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.registration("student@example.com")
	r.BirthDate = nil
	err := e.svc.Register(t.Context(), authz.Anonymous(), r)
	wantErr(t, err, identity.ErrBirthDateRequired)
	wantStatus(t, err, http.StatusUnprocessableEntity)
	if e.hasher.hash.Load() != 0 {
		t.Fatal("an invalid request reached the hasher")
	}
	// Other personas may register without a birth date.
	r.Persona = identity.PersonaCompany
	r.PrivacyNoticeID = e.notices.privacy.ID
	if err := e.svc.Register(t.Context(), authz.Anonymous(), r); !errors.Is(err, identity.ErrPrivacyNoticeInvalid) {
		t.Fatalf("company with a student notice = %v, want identity.privacy_notice_invalid", err)
	}
	// A birth date in the future is refused.
	r = e.registration("future@example.com")
	tomorrow := r.BirthDate.AddDays(365 * 30)
	r.BirthDate = &tomorrow
	wantErr(t, e.svc.Register(t.Context(), authz.Anonymous(), r), identity.ErrInvalidBirthDate)
}

func TestRegister_RecordsConsentInSameTx(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.registration("consent@example.com")
	r.Client = identity.ClientMobile
	r.Consents = []identity.ConsentGrant{{Purpose: consent.PurposeCommercialEmail, NoticeVersionID: e.notices.ccEmail.ID,
		TextSHA256: e.notices.ccEmail.BodySHA256}}
	if err := e.svc.Register(t.Context(), authz.Anonymous(), r); err != nil {
		t.Fatal(err)
	}
	if len(e.consent.calls) != 1 || len(e.store.insertTx) != 1 {
		t.Fatalf("consent calls %d, user inserts %d", len(e.consent.calls), len(e.store.insertTx))
	}
	if e.consent.txs[0] == nil || e.consent.txs[0] != e.store.insertTx[0] {
		t.Fatalf("consent recorded in tx %v, account inserted in tx %v: want the same transaction", e.consent.txs[0], e.store.insertTx[0])
	}
	c := e.consent.calls[0]
	if c.Action != consent.Granted || c.Channel != consent.ChannelMobile || c.Minor || *c.NoticeVersionID != e.notices.ccEmail.ID {
		t.Fatalf("consent input = %+v", c)
	}
	// notice.shown is audited with the account in the same transaction (06 H-42).
	if shown := e.audit.of("notice.shown"); len(shown) != 1 || *shown[0].ResourceID != e.notices.privacy.ID {
		t.Fatalf("notice.shown events = %+v", shown)
	}
	// The consent never rides on the verification e-mail (BE-012): the
	// e-mail is transactional and carries no consent.
	e.deliver(t)
	for _, m := range e.mail.sent() {
		if m.Category != "transactional" {
			t.Fatalf("verification e-mail category %q", m.Category)
		}
	}

	// A consent store failure rolls the whole registration back.
	e2 := newEnv(t)
	e2.consent.err = errors.New("consent store down")
	r2 := e2.registration("rollback@example.com")
	r2.Consents = r.Consents
	r2.Consents[0].NoticeVersionID = e2.notices.ccEmail.ID
	r2.Consents[0].TextSHA256 = e2.notices.ccEmail.BodySHA256
	if err := e2.svc.Register(t.Context(), authz.Anonymous(), r2); err == nil {
		t.Fatal("registration succeeded although the consent failed")
	}
	if len(e2.tx.failed) != 1 || e2.tx.failed[0] != e2.store.insertTx[0] {
		t.Fatalf("failed transactions %v, account insert in %v: the insert must roll back with the consent", e2.tx.failed, e2.store.insertTx)
	}
	if len(e2.out.Events()) != 0 {
		t.Fatal("an event was committed for a rolled-back registration")
	}
}

func TestCreateSession_PendingEmail_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.svc.Register(t.Context(), authz.Anonymous(), e.registration("p@example.com")); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.CreateSession(t.Context(), authz.Anonymous(), identity.Login{Email: "p@example.com", Password: goodPassword})
	wantErr(t, err, identity.ErrEmailNotVerified)
	wantStatus(t, err, http.StatusForbidden)
	// A wrong password on the same account is the generic 401.
	_, err = e.svc.CreateSession(t.Context(), authz.Anonymous(), identity.Login{Email: "p@example.com", Password: "wrong-password-123"})
	wantErr(t, err, identity.ErrInvalidCredentials)
}

func TestCreateSession_WrongPassword_SameTimingPath(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.activeUser(t, "known@example.com")
	ctx := t.Context()
	try := func(email, pw string) (int64, error) {
		before := e.hasher.argon2()
		_, err := e.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: email, Password: pw})
		return e.hasher.argon2() - before, err
	}
	unknownWork, errUnknown := try("unknown@example.com", goodPassword)
	wrongWork, errWrong := try("known@example.com", "not-the-password-1")
	malformedWork, errMalformed := try("not an address", goodPassword)
	for name, c := range map[string]struct {
		work int64
		err  error
	}{"unknown": {unknownWork, errUnknown}, "wrong": {wrongWork, errWrong}, "malformed": {malformedWork, errMalformed}} {
		if c.work != 1 {
			t.Errorf("%s address: %d argon2id runs, want exactly 1", name, c.work)
		}
		if !errors.Is(c.err, identity.ErrInvalidCredentials) {
			t.Errorf("%s address: %v, want identity.invalid_credentials", name, c.err)
		}
	}
	// A locked account costs the same and answers the same.
	for range 5 {
		_, _ = try("known@example.com", "not-the-password-1")
	}
	lockedWork, errLocked := try("known@example.com", goodPassword)
	if lockedWork != 1 || !errors.Is(errLocked, identity.ErrInvalidCredentials) {
		t.Fatalf("locked account: %d runs, %v", lockedWork, errLocked)
	}
	// The lock is per account (05 §14.1): it expires with back-off.
	e.clk.Advance(2 * time.Hour)
	if w, err := try("known@example.com", goodPassword); err != nil || w != 1 {
		t.Fatalf("after the lock expired: %d runs, %v", w, err)
	}
	// Failed logins are audited as denied, without the address.
	denied := e.audit.of(string(authz.IdentitySessionCreate))
	if len(denied) < 6 {
		t.Fatalf("%d session.create audit events", len(denied))
	}
	for _, ev := range denied {
		b, _ := json.Marshal(ev.Metadata)
		hasNoSecret(t, "audit metadata", string(b), "known@example.com", "not-the-password-1", goodPassword)
	}
}

func TestSession_IdleExpirySlides_AbsoluteCaps(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.activeUser(t, "slide@example.com")
	res, _ := e.login(t, "slide@example.com", identity.ClientWeb)
	if got := res.Session.IdleExpiresAt.Sub(res.Session.CreatedAt); got != 7*24*time.Hour {
		t.Fatalf("web idle window %v", got)
	}
	if got := res.Session.AbsoluteExpiresAt.Sub(res.Session.CreatedAt); got != 30*24*time.Hour {
		t.Fatalf("web absolute window %v", got)
	}
	cred := httpx.Credential{Transport: httpx.TransportCookie, TokenHash: [32]byte(token.Hash(res.Token))}
	auth := func() error { _, err := e.svc.Authenticate(t.Context(), cred); return err }

	// Used every 6 days, the session outlives its 7-day idle window…
	for day := 6; day < 30; day += 6 {
		e.clk.Advance(6 * 24 * time.Hour)
		if err := auth(); err != nil {
			t.Fatalf("day %d: %v (the idle window must slide)", day, err)
		}
	}
	s, _ := e.store.Session(t.Context(), res.Session.ID)
	if s.IdleExpiresAt.After(s.AbsoluteExpiresAt) {
		t.Fatal("idle expiry passed the absolute expiry")
	}
	// …but never its 30-day absolute lifetime.
	e.clk.Advance(6 * 24 * time.Hour)
	wantErr(t, auth(), identity.ErrUnauthenticated)

	// Unused for longer than the idle window, a session ends.
	res2, _ := e.login(t, "slide@example.com", identity.ClientMobile)
	if got := res2.Session.IdleExpiresAt.Sub(res2.Session.CreatedAt); got != 30*24*time.Hour {
		t.Fatalf("mobile idle window %v", got)
	}
	e.clk.Advance(31 * 24 * time.Hour)
	_, err := e.svc.Authenticate(t.Context(), httpx.Credential{Transport: httpx.TransportBearer, TokenHash: [32]byte(token.Hash(res2.Token))})
	wantErr(t, err, identity.ErrUnauthenticated)

	// An MFA role caps any session at 30 min idle / 12 h (03 §6.4).
	res3, _ := e.login(t, "slide@example.com", identity.ClientMobile)
	e.members[res3.Session.UserID] = []authz.Membership{{OrgID: uuid.NewV7(), OrgKind: authz.OrgCompany, Role: authz.MemberCompanyAdmin}}
	cred3 := httpx.Credential{Transport: httpx.TransportBearer, TokenHash: [32]byte(token.Hash(res3.Token))}
	e.clk.Advance(29 * time.Minute)
	if _, err := e.svc.Authenticate(t.Context(), cred3); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(31 * time.Minute)
	_, err = e.svc.Authenticate(t.Context(), cred3)
	wantErr(t, err, identity.ErrUnauthenticated)
	res4, _ := e.login(t, "slide@example.com", identity.ClientWeb)
	if got := res4.Session.AbsoluteExpiresAt.Sub(res4.Session.CreatedAt); got != 12*time.Hour {
		t.Fatalf("privileged absolute window %v", got)
	}
}

func TestSession_SuspendedUser_401(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	u := e.activeUser(t, "s@example.com")
	res, _ := e.login(t, "s@example.com", identity.ClientMobile)
	cred := httpx.Credential{Transport: httpx.TransportBearer, TokenHash: [32]byte(token.Hash(res.Token))}
	if _, err := e.svc.Authenticate(t.Context(), cred); err != nil {
		t.Fatal(err)
	}
	// Suspension (BE-016 writes it with suspended_at; the store field is
	// enough here) closes the door at the next request (03 §2.2-3).
	cur, _ := e.store.User(t.Context(), u.ID)
	now := e.clk.Now()
	cur.Status, cur.SuspendedAt = identity.StatusSuspended, &now
	if err := e.store.UpdateUser(t.Context(), cur); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Authenticate(t.Context(), cred)
	wantErr(t, err, identity.ErrUnauthenticated)
	wantStatus(t, err, http.StatusUnauthorized)
	// Nor can it sign in again (the same 401 as a wrong password).
	_, err = e.svc.CreateSession(t.Context(), authz.Anonymous(), identity.Login{Email: "s@example.com", Password: goodPassword})
	wantErr(t, err, identity.ErrInvalidCredentials)
}
