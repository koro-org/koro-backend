package identity_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/sms"
	"koro/internal/platform/sms/smsfake"
)

// smsResetUser is an active account with a verified number whose password
// reset by SMS has been on for SMSResetWait, and its actor.
func (e *env) smsResetUser(t *testing.T, f *smsfake.Fake, email, number string) (identity.User, authz.Actor) {
	t.Helper()
	u := e.activeUser(t, email)
	_, a := e.login(t, email, identity.ClientWeb)
	e.verifiedPhone(t, f, a, number)
	if err := e.svc.EnableSMSReset(t.Context(), a, pwProof); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(identity.SMSResetWait)
	return u, a
}

// resetBySMS asks for a reset with channel sms and reports whether a code left
// by SMS; the answer is the usual one whatever the account is.
func (e *env) resetBySMS(t *testing.T, f *smsfake.Fake, email string) bool {
	t.Helper()
	before := len(f.Sent())
	if err := e.svc.RequestPasswordResetSMS(t.Context(), authz.Anonymous(), email); err != nil {
		t.Fatalf("request = %v, want the usual answer", err)
	}
	e.sendSMS(t)
	return len(f.Sent()) > before
}

// phoneNotice reports whether a phone notice of kind went by e-mail.
func (e *env) phoneNotice(t *testing.T, kind string) bool {
	t.Helper()
	e.runNotices(t)
	for _, m := range e.mail.sent() {
		if m.Template == "identity.phone_notice" && m.Data["kind"] == kind {
			return true
		}
	}
	return false
}

// closedBy returns the records of the reset by SMS closing by itself.
func (e *env) closedBy(cause string) []audit.Event {
	var out []audit.Event
	for _, ev := range e.audit.of(string(authz.IdentitySmsResetDisable)) {
		if ev.Metadata["closed_by"] == cause {
			out = append(out, ev)
		}
	}
	return out
}

// TestSMSReset_OffByDefault: a verified number alone leaves the reset to
// e-mail, however long it has been there; /me says so; a code is refused.
func TestSMSReset_OffByDefault(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	e.activeUser(t, "off@example.com")
	_, a := e.login(t, "off@example.com", identity.ClientWeb)
	e.verifiedPhone(t, f, a, testPhone)
	me, err := e.svc.Me(ctx, a)
	if err != nil || me.Phone == nil || me.Phone.SMSResetEnabled || me.Phone.SMSResetActiveFrom != nil || !me.Phone.SMSResetAllowed {
		t.Fatalf("Me.Phone = %+v, %v", me.Phone, err)
	}
	e.clk.Advance(2 * identity.SMSResetWait)
	if e.resetBySMS(t, f, "off@example.com") {
		t.Fatal("a reset code left with the choice off")
	}
	if tok := e.lastToken(t, "identity.password_reset"); tok == "" {
		t.Fatal("no reset e-mail")
	}
	err = e.svc.ConfirmPasswordResetSMS(ctx, authz.Anonymous(), identity.PasswordResetSMS{
		Email: "off@example.com", Code: "123456", NewPassword: "Yepyeni-Uzun-Parola-2026"})
	problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
}

// TestSMSReset_TurnOnAndOff: it takes a verified number and the proof both
// ways; each change writes a record with the masked number and sends a notice;
// a second call changes nothing.
func TestSMSReset_TurnOnAndOff(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "choice@example.com")
	_, a := e.login(t, "choice@example.com", identity.ClientWeb)
	problemtest.WantErr(t, e.svc.EnableSMSReset(ctx, a, pwProof), identity.ErrPhoneNotVerified)
	if err := e.svc.DisableSMSReset(ctx, a, identity.ReauthProof{}); err != nil {
		t.Fatalf("off without a number = %v", err)
	}
	e.verifiedPhone(t, f, a, testPhone)
	problemtest.WantErr(t, e.svc.EnableSMSReset(ctx, a, identity.ReauthProof{}), identity.ErrReauthRequired)
	problemtest.WantErr(t, e.svc.EnableSMSReset(ctx, a, identity.ReauthProof{Password: "wrong-" + goodPassword}), identity.ErrReauthFailed)
	if d := e.audit.of(string(authz.IdentitySmsResetEnable)); len(d) != 2 || d[0].Outcome != audit.Denied || d[0].Reason != audit.ReasonReauthFailed {
		t.Fatalf("denied records %+v", d)
	}
	if p, _, _ := e.store.Phone(ctx, u.ID); p.SMSResetEnabledAt != nil {
		t.Fatal("on without the proof")
	}
	if err := e.svc.EnableSMSReset(ctx, a, pwProof); err != nil {
		t.Fatal(err)
	}
	at := e.clk.Now()
	me, err := e.svc.Me(ctx, a)
	if err != nil || !me.Phone.SMSResetEnabled || me.Phone.SMSResetActiveFrom == nil || !me.Phone.SMSResetActiveFrom.Equal(at.Add(identity.SMSResetWait)) {
		t.Fatalf("Me.Phone = %+v, %v", me.Phone, err)
	}
	if !e.phoneNotice(t, identity.NoticeSMSResetEnabled) {
		t.Fatal("no notice of turning it on")
	}
	// Already on: nothing changes and the wait does not restart.
	e.clk.Advance(time.Hour)
	if err := e.svc.EnableSMSReset(ctx, a, identity.ReauthProof{}); err != nil {
		t.Fatalf("second enable = %v", err)
	}
	if me, _ := e.svc.Me(ctx, a); !me.Phone.SMSResetActiveFrom.Equal(at.Add(identity.SMSResetWait)) {
		t.Fatalf("the wait moved: %+v", me.Phone)
	}
	ok := e.audit.of(string(authz.IdentitySmsResetEnable))
	if last := ok[len(ok)-1]; len(ok) != 3 || last.Outcome != audit.Success || last.Metadata["phone_hint"] != "***01" || last.Metadata[audit.MetaReauth] != "password" {
		t.Fatalf("records %+v", ok)
	}

	problemtest.WantErr(t, e.svc.DisableSMSReset(ctx, a, identity.ReauthProof{}), identity.ErrReauthRequired)
	if err := e.svc.DisableSMSReset(ctx, a, pwProof); err != nil {
		t.Fatal(err)
	}
	if me, _ := e.svc.Me(ctx, a); me.Phone.SMSResetEnabled || me.Phone.SMSResetActiveFrom != nil {
		t.Fatalf("still on: %+v", me.Phone)
	}
	if !e.phoneNotice(t, identity.NoticeSMSResetDisabled) {
		t.Fatal("no notice of turning it off")
	}
	if err := e.svc.DisableSMSReset(ctx, a, identity.ReauthProof{}); err != nil {
		t.Fatalf("second disable = %v", err)
	}
	for _, ev := range e.audit.events {
		b, _ := json.Marshal(ev)
		hasNoSecret(t, "audit "+ev.Action, string(b), testPhone, "5550000001")
	}

	off := newEnv(t)
	off.activeUser(t, "nosms@example.com")
	_, b := off.login(t, "nosms@example.com", identity.ClientWeb)
	problemtest.WantErr(t, off.svc.EnableSMSReset(ctx, b, pwProof), identity.ErrSMSUnavailable)
}

// TestSMSReset_WorksAfterTheWait: the reset goes by e-mail during the wait and
// by SMS from its end; the code sets the new password and closes the sessions.
// Turning the choice off ends an open code.
func TestSMSReset_WorksAfterTheWait(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "wait@example.com")
	_, a := e.login(t, "wait@example.com", identity.ClientWeb)
	e.verifiedPhone(t, f, a, testPhone)
	if err := e.svc.EnableSMSReset(ctx, a, pwProof); err != nil {
		t.Fatal(err)
	}
	if e.resetBySMS(t, f, "wait@example.com") {
		t.Fatal("a reset code right after turning it on")
	}
	e.clk.Advance(identity.SMSResetWait - time.Second)
	if e.resetBySMS(t, f, "wait@example.com") {
		t.Fatal("a reset code before the end of the wait")
	}
	e.clk.Advance(time.Second)
	if !e.resetBySMS(t, f, "wait@example.com") {
		t.Fatal("no reset code after the wait")
	}
	newPW := "Yepyeni-Uzun-Parola-2026"
	if err := e.svc.ConfirmPasswordResetSMS(ctx, authz.Anonymous(), identity.PasswordResetSMS{
		Email: "wait@example.com", Code: lastSMSCode(t, f, testPhone), NewPassword: newPW}); err != nil {
		t.Fatalf("reset by SMS: %v", err)
	}
	for _, s := range e.sessionsOf(t, u.ID) {
		if s.RevokedAt == nil {
			t.Fatal("a session survived the reset")
		}
	}

	e.clk.Advance(time.Hour)
	_, b := e.smsResetUser(t, f, "open@example.com", testPhone2)
	if !e.resetBySMS(t, f, "open@example.com") {
		t.Fatal("no reset code")
	}
	code := lastSMSCode(t, f, testPhone2)
	if err := e.svc.DisableSMSReset(ctx, b, pwProof); err != nil {
		t.Fatal(err)
	}
	err := e.svc.ConfirmPasswordResetSMS(ctx, authz.Anonymous(), identity.PasswordResetSMS{Email: "open@example.com", Code: code, NewPassword: newPW})
	problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
}

// TestSMSReset_RolesCannotTurnItOn: every org role beyond plain membership and
// every platform role is refused (409, before the proof), from the actor or
// the stored roles; a plain member may.
func TestSMSReset_RolesCannotTurnItOn(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	for i, role := range []authz.MemberRole{authz.MemberClubAdmin, authz.MemberUniversityRep, authz.MemberCareerStaff, authz.MemberCompanyOwner,
		authz.MemberCompanyAdmin, authz.MemberRecruiter, authz.MemberStaff, authz.MemberNGOAdmin, authz.MemberNGOMember} {
		email := fmt.Sprintf("role%d@example.com", i)
		u := e.activeUser(t, email)
		_, a := e.login(t, email, identity.ClientWeb)
		e.verifiedPhone(t, f, a, fmt.Sprintf("+9055500001%02d", i))
		e.clk.Advance(time.Hour) // the SMS limits are not what this test is about
		ms := []authz.Membership{{OrgID: uuid.NewV7(), Role: role}}
		// The stored roles alone refuse it, like the actor's.
		e.members[u.ID] = ms
		problemtest.WantErr(t, e.svc.EnableSMSReset(ctx, a, identity.ReauthProof{}), identity.ErrSMSResetNotAllowed)
		a.Memberships = ms
		problemtest.WantErr(t, e.svc.EnableSMSReset(ctx, a, identity.ReauthProof{}), identity.ErrSMSResetNotAllowed)
		if me, _ := e.svc.Me(ctx, a); me.Phone == nil || me.Phone.SMSResetAllowed {
			t.Fatalf("%s: Me.Phone = %+v", role, me.Phone)
		}
	}

	staff := e.activeUser(t, "staff@example.com")
	_, a := e.login(t, "staff@example.com", identity.ClientWeb)
	e.verifiedPhone(t, f, a, testPhone)
	now := e.clk.Now()
	if err := e.store.InsertRoleAssignment(ctx, identity.RoleAssignment{ID: uuid.NewV7(), UserID: staff.ID, Role: authz.PlatformSupport, Reason: "bootstrap",
		Status: identity.RoleActive, RequestedAt: now, GrantedAt: &now}); err != nil {
		t.Fatal(err)
	}
	problemtest.WantErr(t, e.svc.EnableSMSReset(ctx, a, identity.ReauthProof{}), identity.ErrSMSResetNotAllowed)

	member := e.activeUser(t, "member@example.com")
	_, b := e.login(t, "member@example.com", identity.ClientWeb)
	e.verifiedPhone(t, f, b, testPhone2)
	b.Memberships = []authz.Membership{{OrgID: uuid.NewV7(), OrgKind: authz.OrgClub, Role: authz.MemberMember}}
	e.members[member.ID] = b.Memberships
	if err := e.svc.EnableSMSReset(ctx, b, pwProof); err != nil {
		t.Fatalf("a plain member = %v", err)
	}
}

// membershipStarted runs identity.close_sms_reset for a new membership.
func (e *env) membershipStarted(t *testing.T, userID uuid.UUID, role authz.MemberRole) {
	t.Helper()
	ev, err := domainevent.New(domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: uuid.NewV7(), OrgKind: "club", UserID: userID, Role: string(role)},
		"org_member", uuid.NewV7())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range e.subs.For(domainevent.TopicMembershipStarted) {
		if s.Name == identity.CloseSMSResetTaskName {
			if err := s.Handle(workerCtx(t), ev); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("identity does not subscribe to membership.started")
}

// TestSMSReset_MembershipRoleClosesIt: a new role closes the choice for good: it
// stays closed after the role ends; a plain membership leaves it on. Running
// the task again changes nothing.
func TestSMSReset_MembershipRoleClosesIt(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u, _ := e.smsResetUser(t, f, "club@example.com", testPhone)
	e.membershipStarted(t, u.ID, authz.MemberMember)
	if p, _, _ := e.store.Phone(ctx, u.ID); p.SMSResetEnabledAt == nil {
		t.Fatal("a plain membership closed it")
	}
	e.membershipStarted(t, u.ID, authz.MemberClubAdmin)
	if p, _, _ := e.store.Phone(ctx, u.ID); p.SMSResetEnabledAt != nil {
		t.Fatal("the role left it on")
	}
	e.membershipStarted(t, u.ID, authz.MemberRecruiter)
	rec := e.closedBy("role")
	if len(rec) != 1 || rec[0].SystemJob != identity.CloseSMSResetTaskName || rec[0].Metadata["phone_hint"] != "***01" {
		t.Fatalf("records %+v", rec)
	}
	if !e.phoneNotice(t, identity.NoticeSMSResetDisabled) {
		t.Fatal("no notice of the closing")
	}
	// No role any more (e.members is empty): it does not come back.
	if e.resetBySMS(t, f, "club@example.com") {
		t.Fatal("a reset code after the role ended")
	}
}

// TestSMSReset_RoleWithoutItsEvent: next to a role the choice is not honoured
// even if the task never ran: the request closes it and answers as ever; a
// code asked before the role arrived is refused.
func TestSMSReset_RoleWithoutItsEvent(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u, a := e.smsResetUser(t, f, "late@example.com", testPhone)
	if !e.resetBySMS(t, f, "late@example.com") {
		t.Fatal("no reset code")
	}
	code := lastSMSCode(t, f, testPhone)
	e.members[u.ID] = []authz.Membership{{OrgID: uuid.NewV7(), Role: authz.MemberCareerStaff}}
	err := e.svc.ConfirmPasswordResetSMS(ctx, authz.Anonymous(), identity.PasswordResetSMS{Email: "late@example.com", Code: code, NewPassword: "Yepyeni-Uzun-Parola-2026"})
	problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
	if e.resetBySMS(t, f, "late@example.com") {
		t.Fatal("a reset code next to the role")
	}
	if p, _, _ := e.store.Phone(ctx, u.ID); p.SMSResetEnabledAt != nil {
		t.Fatal("the choice stayed on next to the role")
	}
	if rec := e.closedBy("role"); len(rec) != 1 {
		t.Fatalf("records %+v", rec)
	}
	a.Memberships = e.members[u.ID]
	if me, _ := e.svc.Me(ctx, a); me.Phone.SMSResetEnabled {
		t.Fatalf("Me.Phone = %+v", me.Phone)
	}
	delete(e.members, u.ID)
	if e.resetBySMS(t, f, "late@example.com") {
		t.Fatal("a reset code after the role ended")
	}
}

// TestSMSReset_PlatformRoleClosesIt: a platform role closes the choice when it
// takes effect: at once for a role one admin grants, at the approval of a
// privileged one, not at its request.
func TestSMSReset_PlatformRoleClosesIt(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	seed := func(userID uuid.UUID, number string) {
		t.Helper()
		now := s.clk.Now()
		if err := s.store.PutPhone(ctx, identity.Phone{UserID: userID, Number: number, VerifiedAt: now, SMSResetEnabledAt: &now}); err != nil {
			t.Fatal(err)
		}
	}
	seed(s.target.ID, testPhone)
	if _, err := s.grant(t, s.a, s.target.ID, authz.PlatformSupport); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.store.Phone(ctx, s.target.ID); p.SMSResetEnabledAt != nil {
		t.Fatal("a role in effect left it on")
	}
	rec := s.closedBy("role")
	if len(rec) != 1 || rec[0].ActorUserID == nil || *rec[0].ActorUserID != s.a.UserID {
		t.Fatalf("records %+v", rec)
	}

	other := s.activeUser(t, "other@example.com")
	seed(other.ID, testPhone2)
	ra, err := s.grant(t, s.a, other.ID, authz.PlatformAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.store.Phone(ctx, other.ID); p.SMSResetEnabledAt == nil {
		t.Fatal("a request without an approval closed it")
	}
	if _, err := s.svc.ApprovePlatformRole(ctx, s.b, ra.ID, proof); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.store.Phone(ctx, other.ID); p.SMSResetEnabledAt != nil {
		t.Fatal("the approval left it on")
	}
}

// TestSMSReset_NotNextToTheSMSSecondStep: with the step on the choice is
// refused; turning the step on closes it and turning the step off does not
// bring it back.
func TestSMSReset_NotNextToTheSMSSecondStep(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	e.smsMFAUser(t, f, "step@example.com", testPhone)
	_, a := e.login2(t, f, "step@example.com")
	problemtest.WantErr(t, e.svc.EnableSMSReset(ctx, a, identity.ReauthProof{}), identity.ErrSMSResetNotAllowed)
	if me, _ := e.svc.Me(ctx, a); me.Phone.SMSResetAllowed {
		t.Fatalf("Me.Phone = %+v", me.Phone)
	}

	u, b := e.smsResetUser(t, f, "both@example.com", testPhone2)
	if err := e.svc.EnableSMSMFA(ctx, b, pwProof); err != nil {
		t.Fatal(err)
	}
	p, _, _ := e.store.Phone(ctx, u.ID)
	if p.SMSResetEnabledAt != nil || p.SMSMFAEnabledAt == nil {
		t.Fatalf("phone = %+v", p)
	}
	if rec := e.closedBy("sms_mfa_on"); len(rec) != 1 || rec[0].ActorUserID == nil || *rec[0].ActorUserID != u.ID {
		t.Fatalf("records %+v", rec)
	}
	e.sendSMS(t)
	if _, err := e.svc.SendPhoneChangeCode(ctx, b, pwProof); err != nil {
		t.Fatal(err)
	}
	e.sendSMS(t)
	if err := e.svc.DisableSMSMFA(ctx, b, pwProof, lastSMSCode(t, f, testPhone2)); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := e.store.Phone(ctx, u.ID); p.SMSResetEnabledAt != nil {
		t.Fatal("it came back with the step off")
	}
}

// TestSMSReset_NumberChangeClosesIt: a new number closes the choice; verifying
// the same number again keeps it.
func TestSMSReset_NumberChangeClosesIt(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u, a := e.smsResetUser(t, f, "number@example.com", testPhone)
	on, _, _ := e.store.Phone(ctx, u.ID)
	e.verifiedPhone(t, f, a, testPhone)
	if p, _, _ := e.store.Phone(ctx, u.ID); p.SMSResetEnabledAt == nil || !p.SMSResetEnabledAt.Equal(*on.SMSResetEnabledAt) {
		t.Fatalf("the same number changed the choice: %+v", p)
	}
	e.verifiedPhone(t, f, a, testPhone2)
	if p, _, _ := e.store.Phone(ctx, u.ID); p.Number != testPhone2 || p.SMSResetEnabledAt != nil {
		t.Fatalf("phone = %+v", p)
	}
	if rec := e.closedBy("number_changed"); len(rec) != 1 {
		t.Fatalf("records %+v", rec)
	}
	if !e.phoneNotice(t, identity.NoticeSMSResetDisabled) {
		t.Fatal("no notice of the closing")
	}
	if e.resetBySMS(t, f, "number@example.com") {
		t.Fatal("a reset code to the new number without a new choice")
	}
	v, err := e.svc.Me(ctx, a)
	if err != nil || v.Phone.SMSResetEnabled {
		t.Fatalf("Me.Phone = %+v, %v", v.Phone, err)
	}
}

// TestSMSReset_AnswerAndRecordsHideIt: the request answers alike for every
// account state; the choice and the number are in no sign-in answer or admin
// view; the records and the e-mails hold the masked number only.
func TestSMSReset_AnswerAndRecordsHideIt(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	e.smsResetUser(t, f, "on@example.com", testPhone)
	e.activeUser(t, "plain@example.com")
	_, b := e.login(t, "plain@example.com", identity.ClientWeb)
	e.verifiedPhone(t, f, b, testPhone2)
	e.activeUser(t, "nophone@example.com")
	for _, email := range []string{"on@example.com", "plain@example.com", "nophone@example.com", "nobody@example.com"} {
		if err := e.svc.RequestPasswordResetSMS(ctx, authz.Anonymous(), email); err != nil {
			t.Fatalf("%s: %v", email, err)
		}
	}
	res, err := e.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: "on@example.com", Password: goodPassword, Client: identity.ClientWeb})
	if err != nil {
		t.Fatal(err)
	}
	marshal := func(v any) string {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	hasNoSecret(t, "sign-in answer", marshal(res), testPhone, "5550000001", "SMSReset")
	e.runNotices(t)
	for _, ev := range e.audit.events {
		hasNoSecret(t, "audit "+ev.Action, marshal(ev), testPhone, testPhone2, "5550000001", "5550000002")
	}
	for _, m := range e.mail.sent() {
		hasNoSecret(t, "mail "+m.Template, marshal(m.Data), testPhone, testPhone2, "5550000001", "5550000002")
	}

	s := newStaffEnv(t)
	now := s.clk.Now()
	if err := s.store.PutPhone(ctx, identity.Phone{UserID: s.target.ID, Number: testPhone, VerifiedAt: now, SMSResetEnabledAt: &now}); err != nil {
		t.Fatal(err)
	}
	view, err := s.svc.AdminUser(ctx, s.a, s.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	hasNoSecret(t, "admin view", marshal(view), testPhone, "5550000001", "SMSReset")
}

// TestSMSReset_PrivacyExport: the KVKK export holds the subject's own choice.
func TestSMSReset_PrivacyExport(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	u, _ := e.smsResetUser(t, f, "kvkk@example.com", testPhone)
	on, _, _ := e.store.Phone(t.Context(), u.ID)
	sec, err := e.mod.Privacy.Export(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range sec.Items {
		if it["type"] == "phone" {
			at, ok := it["sms_reset_enabled_at"].(*time.Time)
			if !ok || at == nil || !at.Equal(*on.SMSResetEnabledAt) {
				t.Fatalf("export item %v", it)
			}
			return
		}
	}
	t.Fatalf("export items %v", sec.Items)
}

// TestAdminResetUserMfa_ClosesSMSReset: the admin MFA reset (k2) closes the
// choice; the number stays.
func TestAdminResetUserMfa_ClosesSMSReset(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	now := s.clk.Now()
	if err := s.store.PutPhone(ctx, identity.Phone{UserID: s.target.ID, Number: testPhone, VerifiedAt: now, SMSResetEnabledAt: &now}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.svc.ResetUserMFA(ctx, s.a, s.target.ID, "lost phone, id checked", proof); err != nil || p != nil {
		t.Fatal(err, p)
	}
	p, found, _ := s.store.Phone(ctx, s.target.ID)
	if !found || p.SMSResetEnabledAt != nil || p.Number != testPhone {
		t.Fatalf("phone after the reset = %+v, %v", p, found)
	}
	if rec := s.closedBy("mfa_reset"); len(rec) != 1 {
		t.Fatalf("records %+v", rec)
	}
}

// changeCode asks for the code of the current number and returns it.
func (e *env) changeCode(t *testing.T, f *smsfake.Fake, a authz.Actor, number string) string {
	t.Helper()
	if _, err := e.svc.SendPhoneChangeCode(t.Context(), a, pwProof); err != nil {
		t.Fatalf("SendPhoneChangeCode: %v", err)
	}
	e.sendSMS(t)
	return lastSMSCode(t, f, number)
}

// TestPhoneChange_TakesTheCodeOfTheCurrentNumber: with the SMS second step on
// and the password as the proof, a new number asks for a code sent to the
// current one; a wrong code counts towards the lockout and is recorded; the
// right one works once. The step stays on and follows the new number.
func TestPhoneChange_TakesTheCodeOfTheCurrentNumber(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u := e.smsMFAUser(t, f, "change@example.com", testPhone)
	_, a := e.login2(t, f, "change@example.com")
	for name, err := range map[string]error{
		"start":   func() error { _, err := e.svc.StartPhoneVerification(ctx, a, testPhone2, pwProof, ""); return err }(),
		"remove":  e.svc.RemovePhone(ctx, a, pwProof, ""),
		"disable": e.svc.DisableSMSMFA(ctx, a, pwProof, ""),
	} {
		if !errors.Is(err, identity.ErrPhoneCodeRequired) {
			t.Fatalf("%s without the code = %v", name, err)
		}
	}
	_, err := e.svc.SendPhoneChangeCode(ctx, a, identity.ReauthProof{})
	problemtest.WantErr(t, err, identity.ErrReauthRequired)
	sent, err := e.svc.SendPhoneChangeCode(ctx, a, pwProof)
	if err != nil || sent.PhoneHint != "***01" || !sent.ExpiresAt.Equal(e.clk.Now().Add(10*time.Minute)) {
		t.Fatalf("SendPhoneChangeCode = %+v, %v", sent, err)
	}
	e.sendSMS(t)
	code := lastSMSCode(t, f, testPhone)
	if m, _ := f.Last(testPhone); !strings.HasPrefix(m.Text, "Koro telefon degisikligi onay kodunuz: ") || len(m.Text) > sms.MaxText {
		t.Fatalf("SMS text %q", m.Text)
	}
	wrong := fmt.Sprintf("%06d", (mustAtoi(t, code)+1)%1_000_000)
	_, err = e.svc.StartPhoneVerification(ctx, a, testPhone2, pwProof, wrong)
	problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
	if cur, _ := e.store.User(ctx, u.ID); cur.FailedLogins != 1 {
		t.Fatalf("failed logins = %d, a wrong code must count", cur.FailedLogins)
	}
	d := e.audit.of(string(authz.IdentityPhoneChange))
	if last := d[len(d)-1]; last.Outcome != audit.Denied || last.Reason != "invalid_sms_code" || last.Metadata["sms_purpose"] != "phone_change" {
		t.Fatalf("denied record %+v", last)
	}
	before := len(f.Sent())
	if _, err = e.svc.StartPhoneVerification(ctx, a, testPhone2, pwProof, code[:3]+" "+code[3:]); err != nil {
		t.Fatalf("start with the code = %v", err)
	}
	e.sendSMS(t)
	if len(f.Sent()) != before+1 {
		t.Fatal("no code to the new number")
	}
	ok := e.audit.of(string(authz.IdentityPhoneChange))
	if last := ok[len(ok)-1]; last.Outcome != audit.Success || last.Metadata["phone_change_code_id"] == nil {
		t.Fatalf("record %+v", last)
	}
	// Single use.
	_, err = e.svc.StartPhoneVerification(ctx, a, testPhone2, pwProof, code)
	problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
	// The number changes at the confirmation; the second step follows it.
	if p, _, _ := e.store.Phone(ctx, u.ID); p.Number != testPhone {
		t.Fatalf("number before the confirmation %s", identity.MaskPhone(p.Number))
	}
	v, err := e.svc.ConfirmPhoneVerification(ctx, a, lastSMSCode(t, f, testPhone2))
	if err != nil || !v.SMSMFAEnabled || v.Hint != "***02" {
		t.Fatalf("Confirm = %+v, %v", v, err)
	}
	if ch := e.passwordStep(t, "change@example.com", identity.ClientMobile); ch.PhoneHint != "***02" {
		t.Fatalf("challenge = %+v", ch)
	}
}

// TestPhoneChange_RemoveAndDisableTakeTheCode: removing the number and turning
// the step off take the code as well, and name it in their records.
func TestPhoneChange_RemoveAndDisableTakeTheCode(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u := e.smsMFAUser(t, f, "off.step@example.com", testPhone)
	_, a := e.login2(t, f, "off.step@example.com")
	problemtest.WantErr(t, e.svc.DisableSMSMFA(ctx, a, pwProof, "000000"), identity.ErrInvalidSMSCode)
	if err := e.svc.DisableSMSMFA(ctx, a, pwProof, e.changeCode(t, f, a, testPhone)); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := e.store.Phone(ctx, u.ID); p.SMSMFAEnabledAt != nil || p.Number != testPhone {
		t.Fatalf("phone = %+v", p)
	}
	rec := e.audit.of(string(authz.IdentitySmsMfaDisable))
	if last := rec[len(rec)-1]; last.Outcome != audit.Success || last.Metadata["phone_change_code_id"] == nil {
		t.Fatalf("record %+v", last)
	}

	v := e.smsMFAUser(t, f, "remove.step@example.com", testPhone2)
	_, b := e.login2(t, f, "remove.step@example.com")
	if err := e.svc.RemovePhone(ctx, b, pwProof, e.changeCode(t, f, b, testPhone2)); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := e.store.Phone(ctx, v.ID); found {
		t.Fatal("the number stayed")
	}
}

// TestPhoneChange_TOTPProofIsEnough: on a TOTP account the proof is the TOTP
// code and no code of the number is asked for.
func TestPhoneChange_TOTPProofIsEnough(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u := e.smsMFAUser(t, f, "totp.step@example.com", testPhone)
	_, a := e.login2(t, f, "totp.step@example.com")
	secret, _ := e.enableMFA(t, a)
	e.clk.Advance(time.Minute)
	_, err := e.svc.SendPhoneChangeCode(ctx, a, identity.ReauthProof{TOTPCode: totpCode(t, secret, e.clk.Now())})
	problemtest.WantErr(t, err, identity.ErrPhoneCodeNotRequired)
	e.clk.Advance(time.Minute)
	if err := e.svc.DisableSMSMFA(ctx, a, identity.ReauthProof{TOTPCode: totpCode(t, secret, e.clk.Now())}, ""); err != nil {
		t.Fatalf("disable with a TOTP proof = %v", err)
	}
	if p, _, _ := e.store.Phone(ctx, u.ID); p.SMSMFAEnabledAt != nil {
		t.Fatal("the step stayed on")
	}
}

// TestPhoneChange_NoCodeWithoutTheStep: with the step off nothing but the proof
// is asked for and the code is not sent; without a number, 409.
func TestPhoneChange_NoCodeWithoutTheStep(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	e.activeUser(t, "plain@example.com")
	_, a := e.login(t, "plain@example.com", identity.ClientWeb)
	_, err := e.svc.SendPhoneChangeCode(ctx, a, pwProof)
	problemtest.WantErr(t, err, identity.ErrPhoneNotVerified)
	e.verifiedPhone(t, f, a, testPhone)
	before := len(f.Sent())
	_, err = e.svc.SendPhoneChangeCode(ctx, a, pwProof)
	problemtest.WantErr(t, err, identity.ErrPhoneCodeNotRequired)
	e.sendSMS(t)
	if len(f.Sent()) != before {
		t.Fatal("a code left without a reason")
	}
	if _, err := e.svc.StartPhoneVerification(ctx, a, testPhone2, pwProof, ""); err != nil {
		t.Fatalf("start with the step off = %v", err)
	}
	if err := e.svc.RemovePhone(ctx, a, pwProof, ""); err != nil {
		t.Fatalf("remove with the step off = %v", err)
	}
}

// TestPhoneChange_CodeOfAnotherNumberRefused: a code counts for the number it
// went to only.
func TestPhoneChange_CodeOfAnotherNumberRefused(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u := e.smsMFAUser(t, f, "other@example.com", testPhone)
	_, a := e.login2(t, f, "other@example.com")
	code := e.changeCode(t, f, a, testPhone)
	p, _, _ := e.store.Phone(ctx, u.ID)
	p.Number = testPhone2
	if err := e.store.PutPhone(ctx, p); err != nil {
		t.Fatal(err)
	}
	problemtest.WantErr(t, e.svc.DisableSMSMFA(ctx, a, pwProof, code), identity.ErrInvalidSMSCode)
}

// TestPhoneChange_StepOnEndsAnOpenVerification: a verification started with the
// step off cannot finish once the step is on.
func TestPhoneChange_StepOnEndsAnOpenVerification(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	e.activeUser(t, "open.verify@example.com")
	_, a := e.login(t, "open.verify@example.com", identity.ClientWeb)
	e.verifiedPhone(t, f, a, testPhone)
	if _, err := e.svc.StartPhoneVerification(ctx, a, testPhone2, pwProof, ""); err != nil {
		t.Fatal(err)
	}
	e.sendSMS(t)
	code := lastSMSCode(t, f, testPhone2)
	if err := e.svc.EnableSMSMFA(ctx, a, pwProof); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.ConfirmPhoneVerification(ctx, a, code)
	problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
}

// TestPhone_NumberChangeClosesOpenCodes: a new number closes the open codes of
// every purpose; verifying the same number again leaves them.
func TestPhone_NumberChangeClosesOpenCodes(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "codes@example.com")
	_, a := e.login(t, "codes@example.com", identity.ClientWeb)
	e.verifiedPhone(t, f, a, testPhone)
	seed := func() {
		t.Helper()
		now := e.clk.Now()
		for _, purpose := range []identity.SMSPurpose{identity.SMSLogin, identity.SMSPasswordReset, identity.SMSPhoneChange} {
			c := identity.SMSCode{ID: uuid.NewV7(), UserID: u.ID, Purpose: purpose, Number: testPhone, KeyID: "k1",
				CodeHash: make([]byte, 32), ExpiresAt: now.Add(10 * time.Minute), CreatedAt: now}
			if err := e.store.InsertSMSCode(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
	}
	open := func(purpose identity.SMSPurpose) bool {
		t.Helper()
		_, found, err := e.store.OpenSMSCode(ctx, u.ID, purpose)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	seed()
	e.verifiedPhone(t, f, a, testPhone)
	if !open(identity.SMSLogin) || !open(identity.SMSPasswordReset) || !open(identity.SMSPhoneChange) {
		t.Fatal("the same number closed a code")
	}
	e.verifiedPhone(t, f, a, testPhone2)
	for _, purpose := range []identity.SMSPurpose{identity.SMSPhoneVerify, identity.SMSLogin, identity.SMSPasswordReset, identity.SMSPhoneChange} {
		if open(purpose) {
			t.Fatalf("an open %s code of the old number", purpose)
		}
	}
}

// TestSMSMFA_LoginCodeCountsForItsNumber: a sign-in code counts for the number
// it went to only.
func TestSMSMFA_LoginCodeCountsForItsNumber(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u := e.smsMFAUser(t, f, "login.number@example.com", testPhone)
	ch := e.passwordStep(t, "login.number@example.com", identity.ClientMobile)
	if _, err := e.svc.SendMFALoginSMS(ctx, authz.Anonymous(), ch.ID); err != nil {
		t.Fatal(err)
	}
	e.sendSMS(t)
	code := lastSMSCode(t, f, testPhone)
	p, _, _ := e.store.Phone(ctx, u.ID)
	p.Number = testPhone2
	if err := e.store.PutPhone(ctx, p); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, SMSCode: code})
	problemtest.WantErr(t, err, identity.ErrMFALoginFailed)
}
