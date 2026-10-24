package identity_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/civil"
	"koro/internal/platform/domainevent"
)

// BE-097 (koro-mobile B-06): the registration e-mail of a student's
// .edu.tr address verifies the account and the student in one step.

// signupUser returns the account registered with address.
func (e *eduEnv) signupUser(t *testing.T, address string) identity.User {
	t.Helper()
	id, ok, err := e.svc.FindByEmail(t.Context(), address)
	if err != nil || !ok {
		t.Fatalf("FindByEmail(%s): %v %v", address, ok, err)
	}
	u, err := e.store.User(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// lastAudit is the newest audit event of action.
func (e *eduEnv) lastAudit(t *testing.T, action authz.Action) map[string]any {
	t.Helper()
	evs := e.audit.of(string(action))
	if len(evs) == 0 {
		t.Fatalf("no %s audit", action)
	}
	return evs[len(evs)-1].Metadata
}

// mailsOf counts the e-mails of template.
func (e *eduEnv) mailsOf(template string) int {
	n := 0
	for _, m := range e.mail.sent() {
		if m.Template == template {
			n++
		}
	}
	return n
}

func TestConfirmEmail_EduSignup_VerifiesAccountAndStudent(t *testing.T) {
	t.Parallel()
	var lookups atomic.Int64
	e := newEduEnv(t, func(d *identity.Deps) {
		d.Domains = func(ctx context.Context, domain string) (uuid.UUID, bool, error) {
			lookups.Add(1)
			return fakeDomains(ctx, domain)
		}
	})
	ctx := t.Context()
	const address = "Ada.Yilmaz@Ogr.Uni.edu.tr"
	if err := e.svc.Register(ctx, authz.Anonymous(), e.registration(address)); err != nil {
		t.Fatal(err)
	}
	// Registration does no directory lookup (same work and answer for every
	// address) and sends one e-mail: the account verification, nothing else.
	if n := lookups.Load(); n != 0 {
		t.Fatalf("registration resolved the domain %d times", n)
	}
	tok := e.lastToken(t, "identity.verify_email")
	if sent := e.mail.sent(); len(sent) != 1 || sent[0].ToEmail != address {
		t.Fatalf("e-mails after the registration: %+v", sent)
	}
	u := e.signupUser(t, "ada.yilmaz@ogr.uni.edu.tr")
	if len(e.eduRows(t, u.ID)) != 0 {
		t.Fatal("registration wrote a verification row (the address is not proven yet)")
	}
	// The token is purpose-bound: the student confirmation refuses it and
	// leaves it usable.
	wantErr(t, e.svc.ConfirmEduVerification(ctx, authz.Anonymous(), tok), identity.ErrInvalidToken)

	e.clk.Advance(time.Hour)
	if err := e.svc.ConfirmEmail(ctx, authz.Anonymous(), tok); err != nil {
		t.Fatal(err)
	}
	if cur := e.signupUser(t, "ada.yilmaz@ogr.uni.edu.tr"); cur.Status != identity.StatusActive || cur.EmailVerifiedAt == nil {
		t.Fatalf("account after the confirmation: %+v", cur.Status)
	}
	rows := e.eduRows(t, u.ID)
	if len(rows) != 1 {
		t.Fatalf("verification rows %+v", rows)
	}
	v, now := rows[0], e.clk.Now().UTC().Truncate(time.Microsecond)
	if v.Status != identity.EduVerified || v.Method != identity.EduMethodEmail || *v.UniversityID != eduUni ||
		!v.VerifiedAt.Equal(now) || !v.ExpiresAt.Equal(now.AddDate(1, 0, 0)) || v.EmailDomain != "ogr.uni.edu.tr" ||
		v.EduEmailNormalized != "ada.yilmaz@ogr.uni.edu.tr" || v.EduEmail != address {
		t.Fatalf("verification = %+v", v)
	}
	st, err := e.mod.Students.StudentStatus(ctx, u.ID)
	if err != nil || st.Status != authz.StudentVerified || st.Active.UniversityID != eduUni {
		t.Fatalf("student status = %+v, %v", st, err)
	}
	// user.edu_verified with the payload of confirmEduVerification (the
	// students and BE-077 consumers see the same event).
	evs := e.eduEvents(t, domainevent.TopicUserEduVerified)
	if len(evs) != 1 || evs[0].AggregateID != v.ID {
		t.Fatalf("user.edu_verified events %+v", evs)
	}
	if p := decodeVerified(t, evs[0]); p != (domainevent.UserEduVerified{UserID: u.ID, UniversityID: eduUni, ExpiresAt: *v.ExpiresAt}) {
		t.Fatalf("event = %+v", p)
	}
	if m := e.lastAudit(t, authz.IdentityEduVerificationConfirm); m["via"] != "account_email" || m["renewal"] != false ||
		m["university_id"] != eduUni.String() {
		t.Fatalf("edu audit metadata %v", m)
	}
	if m := e.lastAudit(t, authz.IdentityEmailVerify); m["edu"] != "verified" {
		t.Fatalf("e-mail audit metadata %v", m)
	}
	if n := lookups.Load(); n != 1 {
		t.Fatalf("domain lookups %d, want 1 (at the confirmation)", n)
	}
	// Single use on both endpoints; still one e-mail in total.
	wantErr(t, e.svc.ConfirmEmail(ctx, authz.Anonymous(), tok), identity.ErrInvalidToken)
	wantErr(t, e.svc.ConfirmEduVerification(ctx, authz.Anonymous(), tok), identity.ErrInvalidToken)
	e.deliver(t)
	if n := len(e.mail.sent()); n != 1 || e.mailsOf("identity.edu_verify") != 0 {
		t.Fatalf("%d e-mails, a second verification e-mail went out", n)
	}

	// The year runs like any e-mail verification: a later renewal by the
	// normal path supersedes it.
	_, a := e.login(t, address, identity.ClientWeb)
	e.clk.Advance(330 * 24 * time.Hour)
	e.verifyByEmail(t, a, address)
	if rows := e.eduRows(t, u.ID); len(rows) != 2 || rows[1].Status != identity.EduSuperseded {
		t.Fatalf("rows after the renewal %+v", rows)
	}
	if evs := e.eduEvents(t, domainevent.TopicUserEduVerified); len(evs) != 2 || !decodeVerified(t, evs[1]).Renewal {
		t.Fatalf("renewal events %d", len(evs))
	}
}

// anyAudience lets every persona use the test privacy notice.
type anyAudience struct{ identity.NoticeLookup }

func (n anyAudience) NoticeByID(ctx context.Context, id uuid.UUID) (identity.NoticeInfo, error) {
	info, err := n.NoticeLookup.NoticeByID(ctx, id)
	if info.Kind == consent.KindPrivacyNotice {
		info.Audience = consent.AudienceAll
	}
	return info, err
}

func TestConfirmEmail_EduSignup_OtherwiseAccountOnly(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, address string
		persona       identity.Persona
		noResolver    bool
		wantEdu       any // the "edu" audit metadata (nil: none)
	}{
		{"not edu.tr", "ada@example.com", identity.PersonaStudent, false, nil},
		{"unknown university", "ada@bilinmeyen.edu.tr", identity.PersonaStudent, false, "domain_unknown"},
		{"directory not wired", "ada@ogr.uni.edu.tr", identity.PersonaStudent, true, "domain_unknown"},
		{"club persona", "ada@ogr.uni.edu.tr", identity.PersonaClub, false, nil},
		{"university persona", "ada@uni.edu.tr", identity.PersonaUniversity, false, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEduEnv(t, func(d *identity.Deps) {
				d.Notices = anyAudience{d.Notices}
				if c.noResolver {
					d.Domains = nil
				}
			})
			r := e.registration(c.address)
			r.Persona = c.persona
			if err := e.svc.Register(t.Context(), authz.Anonymous(), r); err != nil {
				t.Fatal(err)
			}
			if err := e.svc.ConfirmEmail(t.Context(), authz.Anonymous(), e.lastToken(t, "identity.verify_email")); err != nil {
				t.Fatalf("ConfirmEmail = %v, want nil (204)", err)
			}
			u := e.signupUser(t, c.address)
			if u.Status != identity.StatusActive {
				t.Fatalf("status %s", u.Status)
			}
			if rows := e.eduRows(t, u.ID); len(rows) != 0 {
				t.Fatalf("verification rows %+v", rows)
			}
			if n := len(e.eduEvents(t, domainevent.TopicUserEduVerified)); n != 0 {
				t.Fatalf("%d user.edu_verified events", n)
			}
			if got := e.lastAudit(t, authz.IdentityEmailVerify)["edu"]; got != c.wantEdu {
				t.Fatalf("edu audit metadata %v, want %v", got, c.wantEdu)
			}
			if n := len(e.audit.of(string(authz.IdentityEduVerificationConfirm))); n != 0 {
				t.Fatalf("%d edu confirm audits", n)
			}
			e.deliver(t)
			if len(e.mail.sent()) != 1 {
				t.Fatalf("e-mails %d, want the verification only", len(e.mail.sent()))
			}
		})
	}
}

func TestConfirmEmail_EduSignup_AddressOfAnotherAccount_AccountOnly(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	ctx := t.Context()
	holder, a := e.student(t, "holder@example.com")
	e.verifyByEmail(t, a, "ada@uni.edu.tr")
	if err := e.svc.Register(ctx, authz.Anonymous(), e.registration("Ada@uni.edu.tr")); err != nil {
		t.Fatal(err)
	}
	// The same 204: the confirmer learns nothing about the other account.
	if err := e.svc.ConfirmEmail(ctx, authz.Anonymous(), e.lastToken(t, "identity.verify_email")); err != nil {
		t.Fatalf("ConfirmEmail = %v, want nil", err)
	}
	u := e.signupUser(t, "ada@uni.edu.tr")
	if u.Status != identity.StatusActive || len(e.eduRows(t, u.ID)) != 0 {
		t.Fatalf("new account: status %s, rows %d", u.Status, len(e.eduRows(t, u.ID)))
	}
	if rows := e.eduRows(t, holder.ID); len(rows) != 1 || rows[0].Status != identity.EduVerified {
		t.Fatalf("the holder's verification changed: %+v", rows)
	}
	if got := e.lastAudit(t, authz.IdentityEmailVerify)["edu"]; got != "address_taken" {
		t.Fatalf("edu audit metadata %v", got)
	}
	if n := len(e.eduEvents(t, domainevent.TopicUserEduVerified)); n != 1 {
		t.Fatalf("%d user.edu_verified events, want the holder's only", n)
	}
}

func TestConfirmEmail_EduSignup_ReissuedTokenVerifiesAccountOnly(t *testing.T) {
	t.Parallel()
	for _, reissue := range []string{"resend", "second registration"} {
		t.Run(reissue, func(t *testing.T) {
			t.Parallel()
			e := newEduEnv(t)
			ctx := t.Context()
			const address = "ada@ogr.uni.edu.tr"
			if err := e.svc.Register(ctx, authz.Anonymous(), e.registration(address)); err != nil {
				t.Fatal(err)
			}
			first := e.lastToken(t, "identity.verify_email")
			var err error
			if reissue == "resend" {
				err = e.svc.ResendVerification(ctx, authz.Anonymous(), address)
			} else {
				err = e.svc.Register(ctx, authz.Anonymous(), e.registration(address))
			}
			if err != nil {
				t.Fatal(err)
			}
			second := e.lastToken(t, "identity.verify_email")
			wantErr(t, e.svc.ConfirmEmail(ctx, authz.Anonymous(), first), identity.ErrInvalidToken) // closed
			if err := e.svc.ConfirmEmail(ctx, authz.Anonymous(), second); err != nil {
				t.Fatal(err)
			}
			u := e.signupUser(t, address)
			if u.Status != identity.StatusActive || len(e.eduRows(t, u.ID)) != 0 {
				t.Fatalf("status %s, rows %d: a re-issued token must prove the account address only", u.Status, len(e.eduRows(t, u.ID)))
			}
			if _, ok := e.lastAudit(t, authz.IdentityEmailVerify)["edu"]; ok {
				t.Fatal("a re-issued token carried the student verification")
			}
			// The normal path stays open.
			_, a := e.login(t, address, identity.ClientMobile)
			e.verifyByEmail(t, a, address)
			if rows := e.eduRows(t, u.ID); len(rows) != 1 || rows[0].Status != identity.EduVerified {
				t.Fatalf("normal path afterwards: %+v", rows)
			}
		})
	}
}

func TestConfirmEmail_EduSignup_ResolverErrorKeepsToken(t *testing.T) {
	t.Parallel()
	down := errors.New("directory down")
	var fail atomic.Bool
	fail.Store(true)
	e := newEduEnv(t, func(d *identity.Deps) {
		d.Domains = func(ctx context.Context, domain string) (uuid.UUID, bool, error) {
			if fail.Load() {
				return uuid.Nil(), false, down
			}
			return fakeDomains(ctx, domain)
		}
	})
	ctx := t.Context()
	if err := e.svc.Register(ctx, authz.Anonymous(), e.registration("ada@uni.edu.tr")); err != nil {
		t.Fatal(err)
	}
	tok := e.lastToken(t, "identity.verify_email")
	if err := e.svc.ConfirmEmail(ctx, authz.Anonymous(), tok); !errors.Is(err, down) {
		t.Fatalf("ConfirmEmail = %v, want the directory error", err)
	}
	if u := e.signupUser(t, "ada@uni.edu.tr"); u.Status != identity.StatusPendingEmail {
		t.Fatalf("status %s after a failed confirmation", u.Status)
	}
	// Nothing was consumed: the same link works once the directory answers.
	fail.Store(false)
	if err := e.svc.ConfirmEmail(ctx, authz.Anonymous(), tok); err != nil {
		t.Fatal(err)
	}
	u := e.signupUser(t, "ada@uni.edu.tr")
	if rows := e.eduRows(t, u.ID); u.Status != identity.StatusActive || len(rows) != 1 {
		t.Fatalf("status %s, rows %d", u.Status, len(rows))
	}
}

func TestConfirmEmail_EduSignup_MinorSameRules(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	ctx := t.Context()
	r := e.registration("genc@ogr.uni.edu.tr")
	b, _ := civil.New(2011, time.March, 1) // 15 at the fake clock
	r.BirthDate = &b
	if err := e.svc.Register(ctx, authz.Anonymous(), r); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ConfirmEmail(ctx, authz.Anonymous(), e.lastToken(t, "identity.verify_email")); err != nil {
		t.Fatal(err)
	}
	u := e.signupUser(t, "genc@ogr.uni.edu.tr")
	if !u.IsMinor(e.clk.Now()) {
		t.Fatal("the fixture is not a minor")
	}
	// A minor is verified like by confirmEduVerification; the minor rules
	// apply downstream (students, outreach, discovery) as for any student.
	rows := e.eduRows(t, u.ID)
	evs := e.eduEvents(t, domainevent.TopicUserEduVerified)
	if len(rows) != 1 || len(evs) != 1 || decodeVerified(t, evs[0]).UserID != u.ID {
		t.Fatalf("rows %d, events %d", len(rows), len(evs))
	}
}

func TestConfirmEmail_EduSignup_AlreadyActiveAccountOnly(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	ctx := t.Context()
	const address = "ada@uni.edu.tr"
	if err := e.svc.Register(ctx, authz.Anonymous(), e.registration(address)); err != nil {
		t.Fatal(err)
	}
	first := e.lastToken(t, "identity.verify_email")
	// A password reset proves the address and activates the account; the
	// registration link clicked afterwards only consumes its token (the
	// student verification rides on the activation alone).
	if err := e.svc.RequestPasswordReset(ctx, authz.Anonymous(), address); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ConfirmPasswordReset(ctx, authz.Anonymous(), e.lastToken(t, "identity.password_reset"), goodPassword+"x"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ConfirmEmail(ctx, authz.Anonymous(), first); err != nil {
		t.Fatal(err)
	}
	u := e.signupUser(t, address)
	if u.Status != identity.StatusActive || len(e.eduRows(t, u.ID)) != 0 {
		t.Fatalf("status %s, rows %d", u.Status, len(e.eduRows(t, u.ID)))
	}
	if _, ok := e.lastAudit(t, authz.IdentityEmailVerify)["edu"]; ok {
		t.Fatal("an already active account got the student step")
	}
}
