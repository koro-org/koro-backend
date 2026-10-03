package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
)

// useMemStore installs the in-memory orgs stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-017.
// Only tickets of the orgs module edit this file (BE-001 R4).
func (w *orgsWiring) useMemStore() {
	w.store = orgs.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
	w.deriver = testDeriver()
}

// TestWire_OrgsProvidesDirectoryPrivacyAndOrgKind pins what orgs binds in
// build: its service, its KVKK provider and entitlements' org kind lookup
// (06 H-40), which answers from the orgs store.
func TestWire_OrgsProvidesDirectoryPrivacyAndOrgKind(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.orgs.svc == nil {
		t.Fatal("orgs service not built")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "orgs" }) {
		t.Fatal("orgs privacy provider not registered")
	}
	if c.entitlements.orgKind == nil {
		t.Fatal("entitlements' org kind lookup is not bound (06 H-40)")
	}
	ctx := t.Context()
	club, err := c.orgs.svc.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Satranç Kulübü"})
	if err != nil {
		t.Fatal(err)
	}
	if kind, found, err := c.entitlements.orgKind(ctx, club); err != nil || !found || kind != "club" {
		t.Fatalf("orgKind(club) = %q, %v, %v", kind, found, err)
	}
	if _, found, err := c.entitlements.orgKind(ctx, uuid.NewV7()); err != nil || found {
		t.Fatalf("orgKind(missing) found=%v err=%v", found, err)
	}
}

// TestWire_OrgsReadsOpenCommerceAtCallTime: a check appended to
// hooks.openCommerce after build (orders and ledger build later) blocks
// closeOrg; the R proof is verified by identity (an account with the
// matrix password).
func TestWire_OrgsReadsOpenCommerceAtCallTime(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	admin, _ := authztest.Get("platform_admin")
	now := c.infra.clock.Now().UTC().Truncate(time.Microsecond)
	email := matrixEmail(admin.Name)
	if err := c.identity.store.InsertUser(ctx, identity.User{
		ID: admin.User(), Status: identity.StatusActive, Email: email, EmailNormalized: email, EmailVerifiedAt: &now,
		PasswordHash: testPasswordHash(), FirstName: "Ayla", LastName: "Admin", Persona: identity.PersonaPlatform,
		Locale: identity.LocaleTR, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	id, err := c.orgs.svc.Create(ctx, orgs.CreateOrg{Kind: orgs.KindCompany, Name: "Kapanan Şirket"})
	if err != nil {
		t.Fatal(err)
	}
	var asked []uuid.UUID
	c.hooks.openCommerce = append(c.hooks.openCommerce, func(_ context.Context, orgID uuid.UUID) (bool, error) {
		asked = append(asked, orgID)
		return true, nil
	})
	proof := identity.ReauthProof{Password: matrixReauthPassword}
	if _, err := c.orgs.svc.CloseOrg(ctx, admin.Actor, id, "Pilot sona erdi", proof); !errors.Is(err, orgs.ErrOpenCommerce) {
		t.Fatalf("closeOrg with open commerce = %v, want orgs.open_commerce", err)
	}
	if len(asked) != 1 || asked[0] != id {
		t.Fatalf("open-commerce hook asked for %v", asked)
	}
	c.hooks.openCommerce = nil
	v, err := c.orgs.svc.CloseOrg(ctx, admin.Actor, id, "Pilot sona erdi", proof)
	if err != nil || v.Org.Status != orgs.StatusClosed {
		t.Fatalf("closeOrg without open commerce = %+v, %v", v.Org, err)
	}
}

// TestActor_MembershipsFromHook: orgs fills hooks.memberships (BE-004 R4),
// so the real ActorResolver carries the user's active org memberships at
// every request (no cache): the founder of a company is its company_owner
// at the next request, and a membership that ends is gone at the next one.
func TestActor_MembershipsFromHook(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	if e.c.hooks.memberships == nil {
		t.Fatal("hooks.memberships is not filled by orgs")
	}
	ctx := t.Context()
	u := e.user(t, nil)
	raw := e.session(t, u, identity.ClientMobile)
	a, err := e.resolve(ctx, httpx.TransportBearer, raw)
	if err != nil || len(a.Memberships) != 0 {
		t.Fatalf("before: %+v, %v", a.Memberships, err)
	}
	founder := authz.Actor{Kind: authz.KindUser, UserID: u.ID, SessionID: a.SessionID, StudentStatus: authz.StudentNone}
	v, err := e.c.orgs.svc.CreateCompany(ctx, founder, orgs.CompanyInput{Name: "Kanca Şirketi", SectorCode: "software", SizeBand: "1-10"})
	if err != nil {
		t.Fatal(err)
	}
	// createCompany closes the founder's sessions (role_change): sign in again.
	raw = e.session(t, u, identity.ClientMobile)
	a, err = e.resolve(ctx, httpx.TransportBearer, raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []authz.Membership{{OrgID: v.Org.ID, OrgKind: authz.OrgCompany, Role: authz.MemberCompanyOwner}}
	if !slices.Equal(a.Memberships, want) {
		t.Fatalf("memberships = %+v, want %+v", a.Memberships, want)
	}
	if !a.HasOrgRole(v.Org.ID, authz.MemberCompanyOwner) {
		t.Fatal("the owner's org surface would be 404")
	}
	// The company's own management surface is reachable now (not 404).
	if _, err := e.c.orgs.svc.ListMembers(ctx, a, v.Org.ID, orgs.MemberQuery{}, orgs.Access{}); err != nil &&
		!errors.Is(err, authz.ErrMFARequired) {
		t.Fatalf("owner's member list: %v", err)
	}
	// A club membership that ends disappears at the next request.
	club, err := e.c.orgs.svc.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Kanca Kulübü"})
	if err != nil {
		t.Fatal(err)
	}
	now := e.clk.Now().UTC()
	if _, err := e.c.orgs.store.InsertMember(ctx, orgs.Membership{ID: uuid.NewV7(), OrgID: club, UserID: u.ID,
		Role: authz.MemberMember, Status: orgs.MemberActive, Source: orgs.SourceInvitation, JoinedAt: &now}); err != nil {
		t.Fatal(err)
	}
	if a, _ = e.resolve(ctx, httpx.TransportBearer, raw); len(a.Memberships) != 2 {
		t.Fatalf("memberships with the club = %+v", a.Memberships)
	}
	if n, err := e.c.orgs.svc.EndRoles(ctx, u.ID, []authz.MemberRole{authz.MemberMember}, orgs.EndClaimRevoked); err != nil || n != 1 {
		t.Fatalf("EndRoles = %d, %v", n, err)
	}
	if a, _ = e.resolve(ctx, httpx.TransportBearer, raw); !slices.Equal(a.Memberships, want) {
		t.Fatalf("memberships after the end = %+v", a.Memberships)
	}
}

// TestWire_OrgsTasksAndInvitationEmail pins what orgs registers (01 §6.2,
// §7) and runs the invitation e-mail through the wired notify service and
// the embedded template.
func TestWire_OrgsTasksAndInvitationEmail(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "orgs.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"orgs.admin_sweep 0 1 * * * default", "orgs.invitations_expire 15 * * * * low"}; !slices.Equal(jobs, want) {
		t.Fatalf("orgs jobs %q, want %q", jobs, want)
	}
	for topic, name := range map[string]string{
		domainevent.TopicOrgInvitationCreated:      orgs.SendInvitationEmailTaskName,
		domainevent.TopicUserEduVerificationLapsed: orgs.EndLapsedAdminRolesTaskName,
		domainevent.TopicStudentGraduated:          orgs.EndGraduatedAdminRolesTaskName,
		domainevent.TopicClubSensitivityChanged:    orgs.PurgeSensitiveMembershipsTaskName,
	} {
		if !slices.ContainsFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == name }) {
			t.Errorf("%s is not subscribed to %s", name, topic)
		}
	}

	ctx := t.Context()
	admin, _ := authztest.Get("platform_admin")
	co, err := c.orgs.svc.Create(ctx, orgs.CreateOrg{Kind: orgs.KindCompany, Name: "E-posta Şirketi"})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := c.orgs.svc.CreateInvitation(ctx, admin.Actor, co, "yeni.kisi@example.com", authz.MemberRecruiter)
	if err != nil {
		t.Fatal(err)
	}
	env, err := domainevent.New(domainevent.OrgInvitationCreated{InvitationID: inv.ID, OrgID: co}, "org_invitation", inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.ID, env.OccurredAt = uuid.NewV7(), c.infra.clock.Now()
	payload, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	sub := c.infra.subs.For(domainevent.TopicOrgInvitationCreated)[0]
	for range 2 { // at-least-once: notify deduplicates
		if err := sub.HandleTask(workerContext(t), payload); err != nil {
			t.Fatalf("%s: %v", sub.Name, err)
		}
	}
	sent := c.notify.transport.(*notify.MemTransport).Sent()
	if len(sent) != 1 || sent[0].To != "yeni.kisi@example.com" ||
		!strings.Contains(sent[0].Text, "https://app.koro.test/invitations/accept#token=") ||
		!strings.Contains(sent[0].Text, "E-posta Şirketi") {
		t.Fatalf("sent %d e-mails", len(sent))
	}
}
