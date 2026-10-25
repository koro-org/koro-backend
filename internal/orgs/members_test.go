package orgs_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/queue"
)

// deliver runs the orgs subscription of topic for every committed event of
// that topic (the worker side; at-least-once: twice).
func (e *env) deliver(t *testing.T, topic string) {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	subs := r.For(topic)
	if len(subs) != 1 {
		t.Fatalf("subscriptions of %s: %d", topic, len(subs))
	}
	for _, ev := range e.out.Events() {
		if ev.Topic != topic {
			continue
		}
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := subs[0].HandleTask(t.Context(), b); err != nil {
				t.Fatalf("%s: %v", subs[0].Name, err)
			}
		}
	}
}

// publish delivers an event of another module to the orgs subscription.
func (e *env) publish(t *testing.T, p domainevent.Payload) error {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	env, err := domainevent.New(p, "user", uuid.NewV7())
	if err != nil {
		t.Fatal(err)
	}
	env.ID, env.OccurredAt = uuid.NewV7(), e.clk.Now()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return r.For(p.Topic())[0].HandleTask(t.Context(), b)
}

func (e *env) job(t *testing.T, name string) queue.Job {
	t.Helper()
	for _, j := range e.mod.Jobs() {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("no job %s", name)
	return queue.Job{}
}

func (e *env) memberOf(t *testing.T, org, id uuid.UUID) (orgs.Membership, bool) {
	t.Helper()
	m, err := e.store.GetMember(t.Context(), org, id)
	if errors.Is(err, orgs.ErrNotFound) {
		return orgs.Membership{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return m, true
}

func TestJoinClub_OpenPolicy_ActiveImmediately(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Açık Kulüp", orgs.JoinOpen, false)
	minor := e.student(true) // T-64: no university match; minors may join as members
	m, err := e.svc.JoinClub(t.Context(), minor, club, "Merhaba")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != orgs.MemberActive || m.JoinedAt == nil || m.RequestedAt == nil || m.Role != authz.MemberMember ||
		m.Source != orgs.SourceJoinRequest || m.OrgKind != orgs.KindClub {
		t.Fatalf("membership = %+v", m)
	}
	if got := e.topics(); !slices.Equal(got, []string{domainevent.TopicMembershipStarted}) {
		t.Fatalf("events %v", got)
	}
	if ev := e.audit.Of(authz.OrgsMemberJoin); len(ev) != 1 || *ev[0].ActorUserID != minor.UserID {
		t.Fatalf("audit %+v", ev)
	}
	// One open membership per user and club.
	_, err = e.svc.JoinClub(t.Context(), minor, club, "")
	problemtest.WantProblem(t, err, 409, "orgs.already_member")
	// The Actor sees it at once (hooks.memberships).
	ms, err := e.svc.MembershipsOf(t.Context(), minor.UserID)
	if err != nil || len(ms) != 1 || ms[0].OrgID != club || ms[0].Role != authz.MemberMember {
		t.Fatalf("MembershipsOf = %+v, %v", ms, err)
	}
	if ok, err := e.svc.HasRole(t.Context(), club, minor.UserID, authz.MemberMember); err != nil || !ok {
		t.Fatalf("HasRole = %v, %v", ok, err)
	}
	if ids, err := e.svc.ActiveMemberIDs(t.Context(), club); err != nil || !slices.Equal(ids, []uuid.UUID{minor.UserID}) {
		t.Fatalf("ActiveMemberIDs = %v, %v", ids, err)
	}
	// A message over 300 characters is refused.
	_, err = e.svc.JoinClub(t.Context(), e.student(false), club, strings.Repeat("a", 301))
	problemtest.WantProblem(t, err, 422, "validation_failed")
}

func TestJoinClub_ApprovalPolicy_Pending(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Onaylı Kulüp", orgs.JoinApproval, false)
	s := e.student(false)
	m, err := e.svc.JoinClub(t.Context(), s, club, "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != orgs.MemberPending || m.JoinedAt != nil || m.RequestedAt == nil {
		t.Fatalf("membership = %+v", m)
	}
	if len(e.topics()) != 0 {
		t.Fatalf("a pending request emitted %v", e.topics())
	}
	if ms, _ := e.svc.MembershipsOf(t.Context(), s.UserID); len(ms) != 0 {
		t.Fatalf("a pending request is an Actor membership: %+v", ms)
	}
	// T-77: without directory's facts the club is not found.
	other := e.club(t, "Kayıtsız", orgs.JoinOpen, false)
	e.clubs.mu.Lock()
	delete(e.clubs.facts, other)
	e.clubs.mu.Unlock()
	_, err = e.svc.JoinClub(t.Context(), s, other, "")
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	// A company is not a club.
	co, _ := e.company(t, "Kulüp Değil")
	_, err = e.svc.JoinClub(t.Context(), s, co.Org.ID, "")
	problemtest.WantErr(t, err, orgs.ErrNotFound)
}

// TestJoinClub_NoProvider_404: with hooks.clubInfo unbound (directory not
// built yet) joinClub is 404 for everyone (T-77 safe default).
func TestJoinClub_NoProvider_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	clk := e.clk
	mod, err := orgs.New(orgs.Deps{Store: e.store, Tx: e.tx, Outbox: e.out, Audit: e.audit, Clock: clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: e.tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()})})
	if err != nil {
		t.Fatal(err)
	}
	club, err := mod.Service.Create(t.Context(), orgs.CreateOrg{Kind: orgs.KindClub, Name: "Sağlayıcısız"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = mod.Service.JoinClub(t.Context(), e.student(false), club, "")
	problemtest.WantErr(t, err, orgs.ErrNotFound)
}

func TestJoinClub_UnverifiedUser_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Doğrulama Kulübü", orgs.JoinOpen, false)
	_, err := e.svc.JoinClub(t.Context(), user(), club, "")
	problemtest.WantProblem(t, err, 403, "students.verification_required")
	lapsed := user()
	lapsed.StudentStatus = authz.StudentLapsed
	_, err = e.svc.JoinClub(t.Context(), lapsed, club, "")
	problemtest.WantProblem(t, err, 403, "students.verification_required")
	// Platform staff are no students either; a missing club stays 404.
	_, err = e.svc.JoinClub(t.Context(), pad(t), club, "")
	problemtest.WantProblem(t, err, 403, "students.verification_required")
	_, err = e.svc.JoinClub(t.Context(), user(), uuid.NewV7(), "")
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	if len(e.audit.Of(authz.OrgsMemberJoin)) != 0 {
		t.Fatal("a refused join was audited as a success")
	}
}

func TestApproveJoinRequest_EmitsMembershipStarted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Onay Kulübü", orgs.JoinApproval, false)
	admin := e.clubAdmin(t, club)
	s := e.student(false)
	req, err := e.svc.JoinClub(t.Context(), s, club, "")
	if err != nil {
		t.Fatal(err)
	}
	// Only the club_admin decides (03 §5.2): a member 403, an outsider 404.
	_, mem := e.join(t, e.club(t, "Başka", orgs.JoinOpen, false), e.student(false))
	_, err = e.svc.ApproveJoinRequest(t.Context(), mem, club, req.ID)
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	member := as(e.student(false), club, orgs.KindClub, authz.MemberMember)
	_, err = e.svc.ApproveJoinRequest(t.Context(), member, club, req.ID)
	problemtest.WantErr(t, err, authz.ErrForbidden)

	v, err := e.svc.ApproveJoinRequest(t.Context(), admin, club, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := v.Membership
	if m.Status != orgs.MemberActive || m.JoinedAt == nil || m.ApprovedBy == nil || *m.ApprovedBy != admin.UserID ||
		v.FirstName != "Ada" {
		t.Fatalf("approved = %+v", v)
	}
	evs := e.out.Events()
	last := evs[len(evs)-1]
	p, err := domainevent.Decode[domainevent.MembershipStarted](last)
	if err != nil || p.MembershipID != req.ID || p.UserID != s.UserID || p.OrgKind != "club" || p.Role != "member" {
		t.Fatalf("membership.started = %+v, %v", p, err)
	}
	if ev := e.audit.Of(authz.OrgsMemberDecide); len(ev) != 1 || ev[0].Metadata["decision"] != "approved" {
		t.Fatalf("audit %+v", ev)
	}
	// Approving again is a state conflict; rejecting an active member too.
	_, err = e.svc.ApproveJoinRequest(t.Context(), admin, club, req.ID)
	problemtest.WantProblem(t, err, 409, "orgs.member_state")
	_, err = e.svc.RejectJoinRequest(t.Context(), admin, club, req.ID)
	problemtest.WantProblem(t, err, 409, "orgs.member_state")

	// A rejected request ends (rejected) and emits nothing.
	s2 := e.student(false)
	req2, _ := e.svc.JoinClub(t.Context(), s2, club, "")
	n := len(e.topics())
	if v, err := e.svc.RejectJoinRequest(t.Context(), admin, club, req2.ID); err != nil || v.Membership.Status != orgs.MemberEnded ||
		*v.Membership.EndReason != orgs.EndRejected {
		t.Fatalf("reject = %+v, %v", v.Membership, err)
	}
	if len(e.topics()) != n {
		t.Fatal("a rejected request emitted an event")
	}
	// H-38: another club's ID in the path finds nothing.
	_, err = e.svc.ApproveJoinRequest(t.Context(), admin, club, uuid.NewV7())
	problemtest.WantErr(t, err, orgs.ErrNotFound)
}

// TestMembership_OrgIsolation_404: a membership of another org addressed
// under this org's path is 404 even for this org's admin (06 H-38).
func TestMembership_OrgIsolation_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.club(t, "Kulüp A", orgs.JoinOpen, false)
	b := e.club(t, "Kulüp B", orgs.JoinOpen, false)
	adminA := e.clubAdmin(t, a)
	mb, _ := e.join(t, b, e.student(false))
	for _, err := range []error{
		func() error { _, err := e.svc.GetMember(t.Context(), adminA, a, mb.ID, orgs.Access{}); return err }(),
		e.svc.RemoveMember(t.Context(), adminA, a, mb.ID, ""),
		func() error {
			_, err := e.svc.ChangeMemberRole(t.Context(), adminA, a, mb.ID, authz.MemberClubAdmin)
			return err
		}(),
	} {
		problemtest.WantErr(t, err, orgs.ErrNotFound)
	}
	// And b's member under b's path is 404 for a's admin (not a member of b).
	_, err := e.svc.GetMember(t.Context(), adminA, b, mb.ID, orgs.Access{})
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	if m, ok := e.memberOf(t, b, mb.ID); !ok || m.Status != orgs.MemberActive {
		t.Fatalf("b's membership changed: %+v", m)
	}
}

func TestLeave_LastAdmin_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Tek Yöneticili", orgs.JoinOpen, false)
	admin := e.clubAdmin(t, club)
	own, err := e.store.LockOpenMember(t.Context(), club, admin.UserID)
	if err != nil {
		t.Fatal(err)
	}
	err = e.svc.LeaveMembership(t.Context(), admin, own.ID)
	problemtest.WantProblem(t, err, 409, "orgs.last_admin")
	// Demoting or removing the last admin is refused too (by a platform admin as well).
	_, err = e.svc.ChangeMemberRole(t.Context(), admin, club, own.ID, authz.MemberMember)
	problemtest.WantProblem(t, err, 409, "orgs.last_admin")
	err = e.svc.RemoveMember(t.Context(), pad(t), club, own.ID, "")
	problemtest.WantProblem(t, err, 409, "orgs.last_admin")

	// With a second admin the first may leave.
	second := e.clubAdmin(t, club)
	if err := e.svc.LeaveMembership(t.Context(), admin, own.ID); err != nil {
		t.Fatalf("leave with a second admin: %v", err)
	}
	if m, _ := e.memberOf(t, club, own.ID); m.Status != orgs.MemberEnded || *m.EndReason != orgs.EndLeft {
		t.Fatalf("left = %+v", m)
	}
	// Another user's membership is 404 on /me; leaving twice is a state conflict.
	err = e.svc.LeaveMembership(t.Context(), second, own.ID)
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	err = e.svc.LeaveMembership(t.Context(), admin, own.ID)
	problemtest.WantProblem(t, err, 409, "orgs.member_state")

	// The company owner never leaves (only a transfer replaces the owner).
	co, owner := e.company(t, "Sahipli Şirket")
	ownerRow, _ := e.store.LockOwner(t.Context(), co.Org.ID)
	err = e.svc.LeaveMembership(t.Context(), owner, ownerRow.ID)
	problemtest.WantProblem(t, err, 409, "orgs.last_admin")
	// A company_admin leaving closes their sessions (MFA role lost, 03 §2.2-6).
	ca := e.member(t, co.Org.ID, authz.MemberCompanyAdmin)
	caRow, _ := e.store.LockOpenMember(t.Context(), co.Org.ID, ca.UserID)
	if err := e.svc.LeaveMembership(t.Context(), ca, caRow.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.revoker.revoked(); !slices.Contains(got, ca.UserID) {
		t.Fatalf("revoked %v, want the company_admin", got)
	}
}

func TestChangeRole_MinorToClubAdmin_Rejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Rol Kulübü", orgs.JoinOpen, false)
	admin := e.clubAdmin(t, club)
	minor, _ := e.join(t, club, e.student(true))
	_, err := e.svc.ChangeMemberRole(t.Context(), admin, club, minor.ID, authz.MemberClubAdmin)
	problemtest.WantProblem(t, err, 422, "orgs.club_admin_requires_adult_student")
	// An adult without a student verification neither.
	unverified := user()
	e.users.add(unverified.UserID, "u@example.com", false)
	now := e.clk.Now()
	um, err := e.store.InsertMember(t.Context(), orgs.Membership{ID: uuid.NewV7(), OrgID: club, UserID: unverified.UserID,
		Role: authz.MemberMember, Status: orgs.MemberActive, Source: orgs.SourceInvitation, JoinedAt: &now})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.ChangeMemberRole(t.Context(), admin, club, um.ID, authz.MemberClubAdmin)
	problemtest.WantProblem(t, err, 422, "orgs.club_admin_requires_adult_student")
	if m, _ := e.memberOf(t, club, minor.ID); m.Status != orgs.MemberActive || m.Role != authz.MemberMember {
		t.Fatalf("a refused change touched the row: %+v", m)
	}

	// An adult student is promoted: the old row ends (role_changed), a new one starts.
	adult, _ := e.join(t, club, e.student(false))
	v, err := e.svc.ChangeMemberRole(t.Context(), admin, club, adult.ID, authz.MemberClubAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if v.Membership.ID == adult.ID || v.Membership.Role != authz.MemberClubAdmin || v.Membership.Status != orgs.MemberActive {
		t.Fatalf("new row = %+v", v.Membership)
	}
	if old, _ := e.memberOf(t, club, adult.ID); old.Status != orgs.MemberEnded || *old.EndReason != orgs.EndRoleChanged {
		t.Fatalf("old row = %+v", old)
	}
	// Roles that do not exist for the org, the owner role and the same role are refused.
	_, err = e.svc.ChangeMemberRole(t.Context(), admin, club, v.Membership.ID, authz.MemberRecruiter)
	problemtest.WantProblem(t, err, 422, "orgs.role_not_allowed")
	_, err = e.svc.ChangeMemberRole(t.Context(), admin, club, v.Membership.ID, authz.MemberClubAdmin)
	problemtest.WantProblem(t, err, 422, "orgs.role_unchanged")
	co, owner := e.company(t, "Rol Şirketi")
	rec := e.member(t, co.Org.ID, authz.MemberRecruiter)
	recRow, _ := e.store.LockOpenMember(t.Context(), co.Org.ID, rec.UserID)
	_, err = e.svc.ChangeMemberRole(t.Context(), owner, co.Org.ID, recRow.ID, authz.MemberCompanyOwner)
	problemtest.WantProblem(t, err, 422, "orgs.role_not_allowed")
	// recruiter → company_admin gains an MFA role: sessions close after the commit.
	if _, err := e.svc.ChangeMemberRole(t.Context(), owner, co.Org.ID, recRow.ID, authz.MemberCompanyAdmin); err != nil {
		t.Fatal(err)
	}
	if got := e.revoker.revoked(); !slices.Contains(got, rec.UserID) {
		t.Fatalf("revoked %v", got)
	}
}

// invite creates an invitation and returns the token the e-mail carries
// (the worker re-derives it).
func (e *env) invite(t *testing.T, a authz.Actor, org uuid.UUID, email string, role authz.MemberRole) (orgs.Invitation, string) {
	t.Helper()
	inv, err := e.svc.CreateInvitation(t.Context(), a, org, email, role)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	e.deliver(t, domainevent.TopicOrgInvitationCreated)
	for _, m := range e.mailer.messages() {
		if m.DedupeKey == "orgs.invitation:"+inv.ID.String() {
			return inv, m.Data["token"].(string)
		}
	}
	t.Fatal("no invitation e-mail")
	return orgs.Invitation{}, ""
}

func TestInvitation_TokenFlow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	co, owner := e.company(t, "Davet Şirketi")
	invitee := user()
	e.users.add(invitee.UserID, "Ayse@Example.com", false)
	inv, raw := e.invite(t, owner, co.Org.ID, "ayse@example.com", authz.MemberRecruiter)
	// Only the hash is stored; the e-mail carries the token and the link.
	if len(inv.TokenHash) != 32 || strings.Contains(string(inv.TokenHash), raw) || len(raw) != 52 {
		t.Fatalf("token %d chars, hash %d bytes", len(raw), len(inv.TokenHash))
	}
	msgs := e.mailer.messages()
	if len(msgs) != 1 || msgs[0].ToEmail != "ayse@example.com" || msgs[0].Template != "orgs.invitation" ||
		msgs[0].Data["link"] != "https://app.koro.test/invitations/accept#token="+raw || msgs[0].Locale != "en" ||
		msgs[0].UserID == nil || *msgs[0].UserID != invitee.UserID {
		t.Fatalf("e-mails %+v", msgs)
	}
	for _, ev := range e.out.Events() {
		if strings.Contains(string(ev.Payload), raw) {
			t.Fatal("the token entered an event")
		}
	}
	if ev := e.audit.Of(authz.OrgsInvitationCreate); len(ev) != 1 || strings.Contains(stringify(ev[0].Metadata), "example.com") {
		t.Fatalf("audit %+v (the address must not be audited)", ev)
	}
	// A second open invitation to the same address is refused.
	_, err := e.svc.CreateInvitation(t.Context(), owner, co.Org.ID, "AYSE@example.com", authz.MemberRecruiter)
	problemtest.WantProblem(t, err, 409, "orgs.invitation_exists")
	// company_owner is never invited; a club role does not fit a company.
	_, err = e.svc.CreateInvitation(t.Context(), owner, co.Org.ID, "b@example.com", authz.MemberCompanyOwner)
	problemtest.WantProblem(t, err, 422, "orgs.role_not_allowed")
	_, err = e.svc.CreateInvitation(t.Context(), owner, co.Org.ID, "not an address", authz.MemberRecruiter)
	problemtest.WantProblem(t, err, 422, "validation_failed")
	if p, err := e.svc.ListInvitations(t.Context(), owner, co.Org.ID, orgs.InvitationQuery{}); err != nil || len(p.Items) != 1 {
		t.Fatalf("list = %+v, %v", p, err)
	}

	// Accept (typed in lower case with spaces: normalised).
	typed := strings.ToLower(raw[:20]) + " " + raw[20:]
	m, err := e.svc.AcceptInvitation(t.Context(), invitee, typed)
	if err != nil {
		t.Fatal(err)
	}
	if m.Role != authz.MemberRecruiter || m.Status != orgs.MemberActive || m.Source != orgs.SourceInvitation ||
		m.InvitedBy == nil || *m.InvitedBy != owner.UserID || m.OrgID != co.Org.ID {
		t.Fatalf("membership = %+v", m)
	}
	// Single use: the same token is gone.
	_, err = e.svc.AcceptInvitation(t.Context(), invitee, raw)
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	if p, _ := e.svc.ListInvitations(t.Context(), owner, co.Org.ID, orgs.InvitationQuery{}); len(p.Items) != 0 {
		t.Fatalf("an accepted invitation is still listed: %+v", p.Items)
	}
	// The member's address cannot be invited to the role it already holds.
	_, err = e.svc.CreateInvitation(t.Context(), owner, co.Org.ID, "ayse@example.com", authz.MemberRecruiter)
	problemtest.WantProblem(t, err, 409, "orgs.already_member")

	// Revoke: the token stops working.
	other := user()
	e.users.add(other.UserID, "mert@example.com", false)
	inv2, raw2 := e.invite(t, owner, co.Org.ID, "mert@example.com", authz.MemberCompanyAdmin)
	if err := e.svc.RevokeInvitation(t.Context(), owner, co.Org.ID, inv2.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AcceptInvitation(t.Context(), other, raw2)
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	err = e.svc.RevokeInvitation(t.Context(), owner, co.Org.ID, inv2.ID)
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	// Garbage tokens are simply not found.
	_, err = e.svc.AcceptInvitation(t.Context(), other, "kısa")
	problemtest.WantErr(t, err, orgs.ErrNotFound)
}

func stringify(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestAcceptInvitation_EmailMismatch_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Davet Kulübü", orgs.JoinApproval, false)
	admin := e.clubAdmin(t, club)
	invitee := e.student(false)
	_, raw := e.invite(t, admin, club, invitee.UserID.String()+"@ogr.example.edu.tr", authz.MemberMember)

	// Another account holding the token (a forwarded e-mail) gets 404 and
	// learns nothing; the invitation stays usable for its addressee.
	thief := e.student(false)
	_, err := e.svc.AcceptInvitation(t.Context(), thief, raw)
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	// An account without an address in identity neither.
	_, err = e.svc.AcceptInvitation(t.Context(), user(), raw)
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	if ms, _ := e.svc.MembershipsOf(t.Context(), thief.UserID); len(ms) != 0 {
		t.Fatalf("the mismatching account became a member: %+v", ms)
	}
	// The addressee (address compared case-insensitively) accepts.
	m, err := e.svc.AcceptInvitation(t.Context(), invitee, raw)
	if err != nil || m.UserID != invitee.UserID {
		t.Fatalf("accept = %+v, %v", m, err)
	}
}

func TestInvitation_Expired_Rejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Süreli Kulüp", orgs.JoinApproval, false)
	admin := e.clubAdmin(t, club)
	invitee := e.student(false)
	addr := invitee.UserID.String() + "@ogr.example.edu.tr"
	inv, raw := e.invite(t, admin, club, addr, authz.MemberMember)
	if !inv.ExpiresAt.Equal(t0.Add(7 * 24 * time.Hour)) {
		t.Fatalf("expires_at = %v, want created + 7 days", inv.ExpiresAt)
	}
	e.clk.Advance(7 * 24 * time.Hour)
	_, err := e.svc.AcceptInvitation(t.Context(), invitee, raw)
	problemtest.WantProblem(t, err, 409, "orgs.invitation_expired")
	// Expired invitations are not listed and get no e-mail.
	if p, _ := e.svc.ListInvitations(t.Context(), admin, club, orgs.InvitationQuery{}); len(p.Items) != 0 {
		t.Fatalf("expired invitation listed: %+v", p.Items)
	}
	// A new invitation to the address replaces the expired one.
	inv2, raw2 := e.invite(t, admin, club, addr, authz.MemberMember)
	if inv2.ID == inv.ID {
		t.Fatal("no new invitation")
	}
	if _, err := e.svc.AcceptInvitation(t.Context(), invitee, raw2); err != nil {
		t.Fatalf("accept the new invitation: %v", err)
	}
	// The hourly job closes expired invitations; the sweeper deletes them
	// 30 days later, only under an approved policy (BE-057).
	other, _ := e.svc.CreateInvitation(t.Context(), admin, club, "x@example.com", authz.MemberMember)
	e.clk.Advance(8 * 24 * time.Hour)
	if err := e.job(t, orgs.InvitationsExpireJobName).Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := e.store.GetInvitation(t.Context(), club, other.ID)
	if err != nil || got.RevokedAt == nil || !got.RevokedAt.Equal(got.ExpiresAt) {
		t.Fatalf("closed = %+v, %v", got, err)
	}
	e.clk.Advance(31 * 24 * time.Hour)
	if err := e.job(t, orgs.InvitationsExpireJobName).Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.GetInvitation(t.Context(), club, other.ID); err != nil {
		t.Fatalf("the job deleted a closed invitation (the sweeper's work): %v", err)
	}
	p := privacy.RetentionPolicy{Code: orgs.PolicyOrgInvitation, Retention: 30 * 24 * time.Hour}
	if res, err := e.mod.Sweeper.Sweep(t.Context(), p, e.clk.Now()); err != nil || res.Count < 1 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if _, err := e.store.GetInvitation(t.Context(), club, other.ID); err != nil {
		t.Fatalf("a dry run deleted the invitation: %v", err)
	}
	p.LegalApproved = true
	if res, err := e.mod.Sweeper.Sweep(t.Context(), p, e.clk.Now()); err != nil || res.Count < 1 || res.Table != "org_invitations" {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	if _, err := e.store.GetInvitation(t.Context(), club, other.ID); !errors.Is(err, orgs.ErrNotFound) {
		t.Fatalf("closed invitation not deleted after its retention: %v", err)
	}
}

func TestAcceptInvitation_ClubAdminNeedsAdultStudent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Yönetici Daveti", orgs.JoinApproval, false)
	admin := e.clubAdmin(t, club)
	minor := e.student(true)
	_, raw := e.invite(t, admin, club, minor.UserID.String()+"@ogr.example.edu.tr", authz.MemberClubAdmin)
	_, err := e.svc.AcceptInvitation(t.Context(), minor, raw)
	problemtest.WantProblem(t, err, 422, "orgs.club_admin_requires_adult_student")
	// A member accepting a club_admin invitation moves up (the old row ends).
	adult := e.student(false)
	m, _ := e.join(t, club, adult)
	_, raw2 := e.invite(t, admin, club, adult.UserID.String()+"@ogr.example.edu.tr", authz.MemberClubAdmin)
	got, err := e.svc.AcceptInvitation(t.Context(), adult, raw2)
	if err != nil || got.Role != authz.MemberClubAdmin {
		t.Fatalf("accept = %+v, %v", got, err)
	}
	if old, _ := e.memberOf(t, club, m.ID); old.Status != orgs.MemberEnded || *old.EndReason != orgs.EndRoleChanged {
		t.Fatalf("old row = %+v", old)
	}
}

func TestEduLapsed_EndsClubAdminRole(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c1 := e.club(t, "Birinci", orgs.JoinOpen, false)
	c2 := e.club(t, "İkinci", orgs.JoinOpen, true)
	admin := e.clubAdmin(t, c1)
	now := e.clk.Now()
	sensRow, err := e.store.InsertMember(t.Context(), orgs.Membership{ID: uuid.NewV7(), OrgID: c2, UserID: admin.UserID,
		Role: authz.MemberClubAdmin, Status: orgs.MemberActive, Source: orgs.SourceClaim, JoinedAt: &now})
	if err != nil {
		t.Fatal(err)
	}
	keep := e.clubAdmin(t, c1) // a second admin of c1: untouched
	// A renewed verification keeps the role (the event came late).
	if err := e.publish(t, domainevent.UserEduVerificationLapsed{UserID: admin.UserID, VerificationID: uuid.NewV7()}); err != nil {
		t.Fatal(err)
	}
	if ms, _ := e.svc.MembershipsOf(t.Context(), admin.UserID); len(ms) != 2 {
		t.Fatalf("roles ended despite a valid verification: %+v", ms)
	}
	e.students.set(admin.UserID, false)
	for range 2 { // at-least-once delivery: idempotent
		if err := e.publish(t, domainevent.UserEduVerificationLapsed{UserID: admin.UserID, VerificationID: uuid.NewV7()}); err != nil {
			t.Fatal(err)
		}
	}
	if ms, _ := e.svc.MembershipsOf(t.Context(), admin.UserID); len(ms) != 0 {
		t.Fatalf("club_admin roles left: %+v", ms)
	}
	row, _ := e.store.LockOpenMember(t.Context(), c1, keep.UserID)
	if row.Status != orgs.MemberActive {
		t.Fatal("another admin's role ended")
	}
	// The sensitive club's row is gone at once; the other one ended (verification_lapsed).
	if _, ok := e.memberOf(t, c2, sensRow.ID); ok {
		t.Fatal("the sensitive club_admin row survived its end")
	}
	ended := e.audit.Of(authz.OrgsMemberEndRole)
	if len(ended) != 2 || ended[0].SystemJob != orgs.EndLapsedAdminRolesTaskName || ended[0].Metadata["end_reason"] != "verification_lapsed" {
		t.Fatalf("audit %+v", ended)
	}
	// student.graduated ends the role as well (graduated).
	g := e.clubAdmin(t, c1)
	if err := e.publish(t, domainevent.StudentGraduated{UserID: g.UserID}); err != nil {
		t.Fatal(err)
	}
	if ms, _ := e.svc.MembershipsOf(t.Context(), g.UserID); len(ms) != 0 {
		t.Fatalf("graduate still club_admin: %+v", ms)
	}
	// A plain membership is no admin role: it stays.
	m, _ := e.join(t, c1, e.student(false))
	e.students.set(m.UserID, false)
	if err := e.publish(t, domainevent.UserEduVerificationLapsed{UserID: m.UserID}); err != nil {
		t.Fatal(err)
	}
	if row, _ := e.memberOf(t, c1, m.ID); row.Status != orgs.MemberActive {
		t.Fatal("a member row ended with the verification")
	}
}

func TestAdminSweep_EndsRolesWithoutVerification(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Süpürme", orgs.JoinOpen, false)
	valid := e.clubAdmin(t, club)
	lapsed := e.clubAdmin(t, club)
	e.students.set(lapsed.UserID, false)
	if err := e.job(t, orgs.AdminSweepJobName).Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.svc.HasRole(t.Context(), club, lapsed.UserID, authz.MemberClubAdmin); ok {
		t.Fatal("the sweep kept a club_admin without a verification")
	}
	if ok, _ := e.svc.HasRole(t.Context(), club, valid.UserID, authz.MemberClubAdmin); !ok {
		t.Fatal("the sweep ended a verified club_admin")
	}
	ev := e.audit.Of(authz.OrgsMemberEndRole)
	if len(ev) != 1 || ev[0].SystemJob != orgs.AdminSweepJobName || ev[0].Metadata["end_reason"] != "verification_lapsed" {
		t.Fatalf("audit %+v", ev)
	}
	// A second run finds nothing more (idempotent).
	if err := e.job(t, orgs.AdminSweepJobName).Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(e.audit.Of(authz.OrgsMemberEndRole)) != 1 {
		t.Fatal("the second sweep ended something")
	}
	if j := e.job(t, orgs.AdminSweepJobName); j.Cron != "0 1 * * *" || j.Queue != queue.Default || j.Unique <= 0 {
		t.Fatalf("job %+v", j)
	}
}

func TestSensitiveClubMembership_DeletedOnEnd(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Hassas Topluluk", orgs.JoinApproval, true)
	admin := e.clubAdmin(t, club)
	ctx := t.Context()
	// leave
	s1 := e.student(false)
	r1, _ := e.svc.JoinClub(ctx, s1, club, "")
	if _, err := e.svc.ApproveJoinRequest(ctx, admin, club, r1.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.LeaveMembership(ctx, as(s1, club, orgs.KindClub, authz.MemberMember), r1.ID); err != nil {
		t.Fatal(err)
	}
	// reject
	r2, _ := e.svc.JoinClub(ctx, e.student(false), club, "")
	if _, err := e.svc.RejectJoinRequest(ctx, admin, club, r2.ID); err != nil {
		t.Fatal(err)
	}
	// remove
	s3 := e.student(false)
	r3, _ := e.svc.JoinClub(ctx, s3, club, "")
	if _, err := e.svc.ApproveJoinRequest(ctx, admin, club, r3.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RemoveMember(ctx, admin, club, r3.ID, "Kural ihlali"); err != nil {
		t.Fatal(err)
	}
	// role change (the old row)
	s4 := e.student(false)
	r4, _ := e.svc.JoinClub(ctx, s4, club, "")
	if _, err := e.svc.ApproveJoinRequest(ctx, admin, club, r4.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ChangeMemberRole(ctx, admin, club, r4.ID, authz.MemberClubAdmin); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{r1.ID, r2.ID, r3.ID, r4.ID} {
		if m, ok := e.memberOf(t, club, id); ok {
			t.Fatalf("ended sensitive membership kept: %+v", m)
		}
	}
	for _, u := range []uuid.UUID{s1.UserID, s3.UserID} {
		if rows, _ := e.store.MembershipsOfUser(ctx, u); len(rows) != 0 {
			t.Fatalf("history of a sensitive membership kept: %+v", rows)
		}
	}
	// membership.ended still tells the consumers (IDs only).
	n := 0
	for _, ev := range e.out.Events() {
		if ev.Topic == domainevent.TopicMembershipEnded {
			n++
		}
	}
	if n != 3 { // leave, remove, role change (a rejected request never started)
		t.Fatalf("membership.ended events = %d", n)
	}
	// Closing the club deletes the open memberships too.
	other := e.student(false)
	e.join(t, club, other)
	if _, err := e.svc.CloseOrg(ctx, pad(t), club, "Topluluk kapandı", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	if rows, _ := e.store.MembershipsOfUser(ctx, other.UserID); len(rows) != 0 {
		t.Fatalf("closing a sensitive club kept %+v", rows)
	}
	// A non-sensitive club keeps the ended row (purged after 6 months, BE-057).
	plain := e.club(t, "Sıradan", orgs.JoinOpen, false)
	pm, pa := e.join(t, plain, e.student(false))
	if err := e.svc.LeaveMembership(ctx, pa, pm.ID); err != nil {
		t.Fatal(err)
	}
	if m, ok := e.memberOf(t, plain, pm.ID); !ok || m.Status != orgs.MemberEnded {
		t.Fatalf("plain ended row = %+v, %v", m, ok)
	}
}

// TestSensitivityChanged_PurgesEndedMemberships: a club that becomes
// sensitive loses its kept ended rows at once (club.sensitivity_changed,
// T-208); open memberships stay; clearing the flag deletes nothing.
func TestSensitivityChanged_PurgesEndedMemberships(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Sonradan Hassas", orgs.JoinOpen, false)
	gone, ga := e.join(t, club, e.student(false))
	if err := e.svc.LeaveMembership(t.Context(), ga, gone.ID); err != nil {
		t.Fatal(err)
	}
	stay, _ := e.join(t, club, e.student(false))
	if err := e.publish(t, domainevent.ClubSensitivityChanged{ClubOrgID: club, Sensitive: false}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.memberOf(t, club, gone.ID); !ok {
		t.Fatal("clearing the flag deleted an ended row")
	}
	for range 2 {
		if err := e.publish(t, domainevent.ClubSensitivityChanged{ClubOrgID: club, Sensitive: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := e.memberOf(t, club, gone.ID); ok {
		t.Fatal("the ended row of a now sensitive club survived")
	}
	if m, ok := e.memberOf(t, club, stay.ID); !ok || m.Status != orgs.MemberActive {
		t.Fatalf("open membership touched: %+v", m)
	}
}

func TestListMembers_MemberRole_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Liste Kulübü", orgs.JoinOpen, false)
	admin := e.clubAdmin(t, club)
	_, member := e.join(t, club, e.student(false))
	// T-20: members do not see each other.
	_, err := e.svc.ListMembers(t.Context(), member, club, orgs.MemberQuery{}, orgs.Access{})
	problemtest.WantErr(t, err, authz.ErrForbidden)
	// An outsider does not see the surface at all.
	_, err = e.svc.ListMembers(t.Context(), e.student(false), club, orgs.MemberQuery{}, orgs.Access{})
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	// The admin does; names come from identity, sort by join date works.
	p, err := e.svc.ListMembers(t.Context(), admin, club, orgs.MemberQuery{Sort: orgs.SortMemberJoined}, orgs.Access{})
	if err != nil || len(p.Items) != 2 || p.Items[0].FirstName != "Ada" {
		t.Fatalf("list = %+v, %v", p, err)
	}
	// A page of one continues with the cursor position.
	p1, err := e.svc.ListMembers(t.Context(), admin, club, orgs.MemberQuery{Limit: 1}, orgs.Access{})
	if err != nil || len(p1.Items) != 1 || p1.Next == nil {
		t.Fatalf("page 1 = %+v, %v", p1, err)
	}
	p2, err := e.svc.ListMembers(t.Context(), admin, club, orgs.MemberQuery{Limit: 1, After: p1.Next}, orgs.Access{})
	if err != nil || len(p2.Items) != 1 || p2.Items[0].Membership.ID == p1.Items[0].Membership.ID || p2.Next != nil {
		t.Fatalf("page 2 = %+v, %v", p2, err)
	}
	// Platform staff read only by break-glass: without a reason 403, with one 200 + audit.
	_, err = e.svc.ListMembers(t.Context(), persona(t, "support"), club, orgs.MemberQuery{}, orgs.Access{})
	problemtest.WantErr(t, err, authz.ErrBreakGlassReason)
	if _, err := e.svc.ListMembers(t.Context(), persona(t, "support"), club, orgs.MemberQuery{},
		orgs.Access{Reason: "Destek talebi 4711 inceleniyor"}); err != nil {
		t.Fatal(err)
	}
	ev := e.audit.Of(authz.OrgsMemberList)
	if len(ev) != 1 || ev[0].Metadata[audit.MetaBreakGlass] != true || ev[0].Reason == "" {
		t.Fatalf("break-glass audit %+v", ev)
	}
}

// TestListMembers_AvatarFromStudents: the member list carries the
// students module's avatar URL (04 §2.2 MemberDTO.user.avatar_url; BE-023);
// an avatar failure never fails the read.
func TestListMembers_AvatarFromStudents(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Avatar Kulübü", orgs.JoinOpen, false)
	admin := e.clubAdmin(t, club)
	_, member := e.join(t, club, e.student(false))
	e.avatars.mu.Lock()
	e.avatars.urls[member.UserID] = "https://files.test/avatar"
	e.avatars.mu.Unlock()
	p, err := e.svc.ListMembers(t.Context(), admin, club, orgs.MemberQuery{}, orgs.Access{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]string{}
	for _, v := range p.Items {
		got[v.Membership.UserID] = v.AvatarURL
	}
	if got[member.UserID] != "https://files.test/avatar" || got[admin.UserID] != "" {
		t.Fatalf("avatars = %v", got)
	}
	e.avatars.mu.Lock()
	e.avatars.fail = errors.New("students down")
	e.avatars.mu.Unlock()
	if p, err := e.svc.ListMembers(t.Context(), admin, club, orgs.MemberQuery{}, orgs.Access{}); err != nil || len(p.Items) != 2 ||
		p.Items[0].AvatarURL != "" || p.Items[0].FirstName == "" {
		t.Fatalf("list with failing avatars = %+v, %v", p, err)
	}
}

func TestSensitiveClubMemberList_Audited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "İnanç Topluluğu", orgs.JoinOpen, true)
	admin := e.clubAdmin(t, club)
	m, member := e.join(t, club, e.student(false))
	for range 2 {
		p, err := e.svc.ListMembers(t.Context(), admin, club, orgs.MemberQuery{}, orgs.Access{})
		if err != nil || len(p.Items) != 2 {
			t.Fatalf("list = %+v, %v", p, err)
		}
	}
	ev := e.audit.Of(authz.OrgsMemberList)
	if len(ev) != 2 || ev[0].Metadata[audit.MetaSensitive] != true || *ev[0].OrgID != club || *ev[0].ActorUserID != admin.UserID {
		t.Fatalf("audit %+v", ev)
	}
	// Y2: platform staff see nothing, even with a reason; a member is 403.
	for _, name := range []string{"support", "platform_admin", "dpo"} {
		_, err := e.svc.ListMembers(t.Context(), persona(t, name), club, orgs.MemberQuery{},
			orgs.Access{Reason: "Destek talebi 4711 inceleniyor"})
		problemtest.WantErr(t, err, orgs.ErrNotFound)
		_, err = e.svc.GetMember(t.Context(), persona(t, name), club, m.ID, orgs.Access{Reason: "Destek talebi 4711 inceleniyor"})
		problemtest.WantErr(t, err, orgs.ErrNotFound)
	}
	_, err := e.svc.ListMembers(t.Context(), member, club, orgs.MemberQuery{}, orgs.Access{})
	problemtest.WantErr(t, err, authz.ErrForbidden)
	// A single sensitive membership read is audited as well.
	if _, err := e.svc.GetMember(t.Context(), admin, club, m.ID, orgs.Access{}); err != nil {
		t.Fatal(err)
	}
	if ev := e.audit.Of(authz.OrgsMemberRead); len(ev) != 1 || ev[0].Metadata[audit.MetaSensitive] != true {
		t.Fatalf("read audit %+v", ev)
	}
	// Unknown sensitivity (no directory facts) counts as sensitive.
	e.clubs.mu.Lock()
	delete(e.clubs.facts, club)
	e.clubs.mu.Unlock()
	_, err = e.svc.ListMembers(t.Context(), persona(t, "platform_admin"), club, orgs.MemberQuery{},
		orgs.Access{Reason: "Destek talebi 4711 inceleniyor"})
	problemtest.WantErr(t, err, orgs.ErrNotFound)
}

// TestOrgAdmin_GrantAndEndRoles: directory's claim approval grants
// club_admin in its transaction (adult verified student only) and a claim
// revocation ends it.
func TestOrgAdmin_GrantAndEndRoles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.club(t, "Sahiplenilen", orgs.JoinOpen, false)
	s := e.student(false)
	m, _ := e.join(t, club, s)
	if err := e.svc.GrantRole(t.Context(), club, s.UserID, authz.MemberClubAdmin, "claim approved"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.GrantRole(t.Context(), club, s.UserID, authz.MemberClubAdmin, "again"); err != nil {
		t.Fatalf("idempotent grant: %v", err)
	}
	if ok, _ := e.svc.HasRole(t.Context(), club, s.UserID, authz.MemberClubAdmin); !ok {
		t.Fatal("club_admin not granted")
	}
	if old, _ := e.memberOf(t, club, m.ID); old.Status != orgs.MemberEnded {
		t.Fatalf("the member row did not end: %+v", old)
	}
	err := e.svc.GrantRole(t.Context(), club, e.student(true).UserID, authz.MemberClubAdmin, "minor")
	problemtest.WantProblem(t, err, 422, "orgs.club_admin_requires_adult_student")
	err = e.svc.GrantRole(t.Context(), club, s.UserID, authz.MemberCompanyOwner, "")
	problemtest.WantProblem(t, err, 422, "orgs.role_not_allowed")
	n, err := e.svc.EndRoles(t.Context(), s.UserID, []authz.MemberRole{authz.MemberClubAdmin}, orgs.EndClaimRevoked)
	if err != nil || n != 1 {
		t.Fatalf("EndRoles = %d, %v", n, err)
	}
	if ok, _ := e.svc.HasRole(t.Context(), club, s.UserID, authz.MemberClubAdmin); ok {
		t.Fatal("club_admin not ended")
	}
}

// TestPrivacy_MembershipsAndInvitations: export holds the memberships and
// invitations of the user; erase ends the open memberships
// (account_erased), deletes sensitive ones and the invitations.
func TestPrivacy_MembershipsAndInvitations(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	plain := e.club(t, "Açık", orgs.JoinOpen, false)
	sens := e.club(t, "Hassas", orgs.JoinOpen, true)
	admin := e.clubAdmin(t, plain)
	s := e.student(false)
	pm, _ := e.join(t, plain, s)
	e.join(t, sens, s)
	inv, _ := e.invite(t, admin, plain, s.UserID.String()+"@ogr.example.edu.tr", authz.MemberClubAdmin)

	sec, err := e.mod.Privacy.Export(t.Context(), s.UserID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, it := range sec.Items {
		kinds[it["record"].(string)]++
	}
	if kinds["membership"] != 2 || kinds["invitation"] != 1 {
		t.Fatalf("export %v", kinds)
	}
	res, err := e.mod.Privacy.Erase(t.Context(), s.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 2 || res.Retained != 1 || res.RetainedReason == "" { // sensitive row + invitation; plain row kept
		t.Fatalf("erase = %+v", res)
	}
	if m, _ := e.memberOf(t, plain, pm.ID); m.Status != orgs.MemberEnded || *m.EndReason != orgs.EndAccountErased {
		t.Fatalf("plain row = %+v", m)
	}
	if _, err := e.store.GetInvitation(t.Context(), plain, inv.ID); !errors.Is(err, orgs.ErrNotFound) {
		t.Fatalf("invitation kept: %v", err)
	}
	again, err := e.mod.Privacy.Erase(t.Context(), s.UserID)
	if err != nil || again.Deleted != 0 {
		t.Fatalf("second erase = %+v, %v", again, err)
	}
	if e.mod.Privacy.Name() != "orgs" {
		t.Fatal("provider name")
	}
}
