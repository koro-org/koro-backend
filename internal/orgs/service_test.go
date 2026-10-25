package orgs_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/db/dbtest"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem/problemtest"
)

func TestCreateCompany_CreatorBecomesOwner_SessionsRevoked(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	founder := user()
	v, err := e.svc.CreateCompany(t.Context(), founder, companyInput("Acme Yazılım"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := e.store.LockOwner(t.Context(), v.Org.ID)
	if err != nil || owner.UserID != founder.UserID || owner.Status != orgs.MemberActive ||
		owner.OrgKind != orgs.KindCompany || owner.JoinedAt == nil {
		t.Fatalf("owner row = %+v, %v", owner, err)
	}
	if v.Org.CreatedBy == nil || *v.Org.CreatedBy != founder.UserID {
		t.Fatalf("created_by = %v", v.Org.CreatedBy)
	}
	// 03 §2.2-6: the founder's sessions are closed with role_change, after
	// the commit (identity.Sessions.RevokeAll is not a same-transaction write).
	if got := e.revoker.revoked(); !slices.Equal(got, []uuid.UUID{founder.UserID}) {
		t.Fatalf("revoked sessions of %v, want the founder", got)
	}
	if e.revoker.inTx {
		t.Fatal("sessions were revoked inside the business transaction")
	}
	if got := e.topics(); !slices.Equal(got, []string{domainevent.TopicMembershipStarted}) {
		t.Fatalf("events %v, want membership.started", got)
	}
	if ev := e.audit.Of(authz.OrgsCompanyCreate); len(ev) != 1 || *ev[0].OrgID != v.Org.ID || *ev[0].ActorUserID != founder.UserID {
		t.Fatalf("audit = %+v", ev)
	}
	// A failed company (slug taken) revokes nothing and writes no owner.
	other := user()
	in := companyInput("Başka Şirket")
	in.Slug = v.Org.Slug
	_, err = e.svc.CreateCompany(t.Context(), other, in)
	problemtest.WantProblem(t, err, 409, "orgs.slug_taken")
	if got := e.revoker.revoked(); len(got) != 1 {
		t.Fatalf("a failed registration revoked sessions: %v", got)
	}
}

func TestCreateCompany_StartsPendingReview(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v, _ := e.company(t, "Çağla Işık Danışmanlık")
	o := v.Org
	if o.Status != orgs.StatusPendingReview || o.Verified() || o.Kind != orgs.KindCompany {
		t.Fatalf("new company = %+v", o)
	}
	if o.Slug != "cagla-isik-danismanlik" {
		t.Fatalf("slug = %q", o.Slug)
	}
	if v.Company == nil || *v.Company.SectorCode != "software" || *v.Company.SizeBand != "11-50" ||
		v.Company.OrgKind != orgs.KindCompany {
		t.Fatalf("company profile = %+v", v.Company)
	}
	// A second company of the same name gets the next free slug.
	v2, _ := e.company(t, "Çağla Işık Danışmanlık")
	if v2.Org.Slug != "cagla-isik-danismanlik-2" {
		t.Fatalf("second slug = %q", v2.Org.Slug)
	}
	// pending_review is hidden from the public, visible to members and staff
	// (03 §3.6).
	_, err := e.svc.GetOrg(t.Context(), authz.Anonymous(), o.ID)
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	_, err = e.svc.GetOrg(t.Context(), user(), o.ID)
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	if _, err := e.svc.GetOrg(t.Context(), pad(t), o.ID); err != nil {
		t.Fatalf("platform_admin reads a pending company: %v", err)
	}
	// Not listed in the public search.
	p, err := e.svc.SearchOrgs(t.Context(), authz.Anonymous(), orgs.SearchQuery{Q: "Danışmanlık"})
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("search found pending companies: %d, %v", len(p.Items), err)
	}
	// Validation: sector and size are required; minors and staff cannot register.
	bad := companyInput("Eksik")
	bad.SizeBand = ""
	_, err = e.svc.CreateCompany(t.Context(), user(), bad)
	problemtest.WantProblem(t, err, 422, "validation_failed")
	_, err = e.svc.CreateCompany(t.Context(), persona(t, "student_minor"), companyInput("Reşit Değil"))
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.CreateCompany(t.Context(), pad(t), companyInput("Personel"))
	problemtest.WantErr(t, err, authz.ErrForbidden)
}

func TestUpdateOrg_StaleETag_412(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v, owner := e.company(t, "Etag Şirketi")
	desc := "İlk açıklama"
	p := orgs.OrgPatch{Description: &desc}

	_, err := e.svc.UpdateOrg(t.Context(), owner, v.Org.ID, "", p)
	problemtest.WantProblem(t, err, 428, "precondition_required")
	_, err = e.svc.UpdateOrg(t.Context(), owner, v.Org.ID, "*", p)
	problemtest.WantProblem(t, err, 428, "precondition_required")

	got, err := e.svc.UpdateOrg(t.Context(), owner, v.Org.ID, v.ETag, p)
	if err != nil || got.Org.Description == nil || *got.Org.Description != desc {
		t.Fatalf("update with the current ETag = %+v, %v", got.Org, err)
	}
	if got.ETag == v.ETag {
		t.Fatal("the ETag did not change with the update")
	}
	// The old ETag is stale now: a concurrent editor gets 412 and nothing changes.
	other := "Kayıp güncelleme"
	_, err = e.svc.UpdateOrg(t.Context(), owner, v.Org.ID, v.ETag, orgs.OrgPatch{Description: &other})
	problemtest.WantProblem(t, err, 412, "precondition_failed")
	cur, err := e.svc.GetOrg(t.Context(), owner, v.Org.ID)
	if err != nil || *cur.Org.Description != desc || cur.ETag != got.ETag {
		t.Fatalf("after the 412: %+v (etag %s), %v", cur.Org, cur.ETag, err)
	}
	// A list of tags matches if one of them is current.
	size := "51-200"
	if _, err := e.svc.UpdateOrg(t.Context(), owner, v.Org.ID, `"x", `+got.ETag, orgs.OrgPatch{SizeBand: &size}); err != nil {
		t.Fatalf("If-Match list: %v", err)
	}
	if ev := e.audit.Of(authz.OrgsOrgUpdate); len(ev) != 2 {
		t.Fatalf("update audit records = %d, want 2", len(ev))
	}
}

func TestUpdateOrg_OrgIsolation_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a, ownerA := e.company(t, "A Şirketi")
	b, _ := e.company(t, "B Şirketi")
	name := "Ele Geçirildi"
	// The owner of A on B's management surface: 404 (03 §3.6), and B is unchanged.
	_, err := e.svc.UpdateOrg(t.Context(), ownerA, b.Org.ID, b.ETag, orgs.OrgPatch{Name: &name})
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	_, err = e.svc.CloseOrg(t.Context(), ownerA, b.Org.ID, "kapat", proof(goodPassword))
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	_, err = e.svc.TransferOwnership(t.Context(), ownerA, b.Org.ID, ownerA.UserID, proof(goodPassword))
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	// A recruiter of A: member without the role → 403.
	rec := e.member(t, a.Org.ID, authz.MemberRecruiter)
	_, err = e.svc.UpdateOrg(t.Context(), rec, a.Org.ID, a.ETag, orgs.OrgPatch{Name: &name})
	problemtest.WantErr(t, err, authz.ErrForbidden)
	if e.reauth.calls != 0 {
		t.Fatal("re-authentication ran before authz")
	}
	// 06 H-38: a membership takes org_kind from its org, whatever the caller says.
	now := e.clk.Now()
	m, err := e.store.InsertMember(t.Context(), orgs.Membership{
		ID: uuid.NewV7(), OrgID: a.Org.ID, OrgKind: orgs.KindClub, UserID: uuid.NewV7(), Role: authz.MemberRecruiter,
		Status: orgs.MemberActive, Source: orgs.SourceAdmin, JoinedAt: &now,
	})
	if err != nil || m.OrgKind != orgs.KindCompany {
		t.Fatalf("membership org_kind = %q, %v", m.OrgKind, err)
	}
	// Company fields on a club are refused.
	club, err := e.svc.Create(t.Context(), orgs.CreateOrg{Kind: orgs.KindClub, Name: "Satranç Kulübü"})
	if err != nil {
		t.Fatal(err)
	}
	cv, err := e.svc.GetOrg(t.Context(), pad(t), club)
	if err != nil {
		t.Fatal(err)
	}
	sector := "software"
	_, err = e.svc.UpdateOrg(t.Context(), pad(t), club, cv.ETag, orgs.OrgPatch{SectorCode: &sector})
	problemtest.WantErr(t, err, orgs.ErrCompanyFieldOnly)
}

func TestTransferOwnership_TargetMustBeActiveCompanyAdmin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v, owner := e.company(t, "Devir Şirketi")
	org := v.Org.ID
	recruiter := e.member(t, org, authz.MemberRecruiter)
	admin := e.member(t, org, authz.MemberCompanyAdmin)
	ctx := t.Context()

	for _, target := range []uuid.UUID{recruiter.UserID, uuid.NewV7(), owner.UserID} {
		_, err := e.svc.TransferOwnership(ctx, owner, org, target, proof(goodPassword))
		problemtest.WantProblem(t, err, 422, "orgs.transfer_target_invalid")
	}
	// company_admin is not the owner (k-own); a wrong proof is 403.
	_, err := e.svc.TransferOwnership(ctx, admin, org, admin.UserID, proof(goodPassword))
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.TransferOwnership(ctx, owner, org, admin.UserID, proof("yanlis"))
	problemtest.WantProblem(t, err, 403, "identity.reauth_failed")

	e.revoker.users = nil
	if _, err := e.svc.TransferOwnership(ctx, owner, org, admin.UserID, proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	newOwner, err := e.store.LockOwner(ctx, org)
	if err != nil || newOwner.UserID != admin.UserID {
		t.Fatalf("owner after transfer = %+v, %v", newOwner, err)
	}
	former, err := e.store.LockActiveMember(ctx, org, owner.UserID)
	if err != nil || former.Role != authz.MemberCompanyAdmin {
		t.Fatalf("former owner = %+v, %v", former, err)
	}
	if got := e.revoker.revoked(); !slices.Contains(got, admin.UserID) || !slices.Contains(got, owner.UserID) {
		t.Fatalf("revoked %v, want both users", got)
	}
	if n := len(e.audit.OfOutcome(authz.OrgsOwnershipTransfer, audit.Denied)); n != 1 {
		t.Fatalf("%d denied records for the wrong proof, want 1 (03 §6.2, BE-074)", n)
	}
	ev := e.audit.OfOutcome(authz.OrgsOwnershipTransfer, audit.Success)
	if len(ev) != 1 || ev[0].Metadata["reauth"] != "password" || ev[0].Metadata["new_owner_user_id"] != admin.UserID.String() {
		t.Fatalf("audit = %+v", ev)
	}
	// History is kept: two ended (role_changed) rows, two new ones.
	rows, _ := e.store.MembershipsOfUser(ctx, owner.UserID)
	if len(rows) != 2 || rows[0].Status != orgs.MemberEnded || *rows[0].EndReason != orgs.EndRoleChanged {
		t.Fatalf("former owner's rows = %+v", rows)
	}
	// A club has no owner to transfer.
	club, _ := e.svc.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Kulüp"})
	_, err = e.svc.TransferOwnership(ctx, pad(t), club, admin.UserID, proof(goodPassword))
	problemtest.WantProblem(t, err, 409, "orgs.invalid_state")
}

func TestCloseOrg_HookReportsOpenCommerce_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v, owner := e.company(t, "Kapanacak Şirket")
	member := e.member(t, v.Org.ID, authz.MemberRecruiter)
	var asked []uuid.UUID
	open := true
	e.open = []orgs.OpenCommerceCheck{
		func(context.Context, uuid.UUID) (bool, error) { return false, nil }, // ledger: no payout
		func(_ context.Context, id uuid.UUID) (bool, error) { asked = append(asked, id); return open, nil },
	}
	_, err := e.svc.CloseOrg(t.Context(), owner, v.Org.ID, "Faaliyet sona erdi", proof(goodPassword))
	problemtest.WantProblem(t, err, 409, "orgs.open_commerce")
	if len(asked) != 1 || asked[0] != v.Org.ID {
		t.Fatalf("hook asked %v", asked)
	}
	cur, _ := e.store.GetOrg(t.Context(), v.Org.ID)
	if cur.Status != orgs.StatusPendingReview {
		t.Fatalf("status after a refused close = %s", cur.Status)
	}
	// A failing hook fails the close too.
	e.open = []orgs.OpenCommerceCheck{func(context.Context, uuid.UUID) (bool, error) { return false, errors.New("orders down") }}
	if _, err := e.svc.CloseOrg(t.Context(), owner, v.Org.ID, "Faaliyet sona erdi", proof(goodPassword)); err == nil {
		t.Fatal("close succeeded with a failing open-commerce check")
	}
	// No hook (P1: orders and ledger not built) → no check.
	e.open = nil
	got, err := e.svc.CloseOrg(t.Context(), owner, v.Org.ID, "Faaliyet sona erdi", proof(goodPassword))
	if err != nil || got.Org.Status != orgs.StatusClosed || got.Org.ClosedAt == nil {
		t.Fatalf("close = %+v, %v", got.Org, err)
	}
	// Every membership ended (org_closed); closing again is 409.
	if _, err := e.store.LockActiveMember(t.Context(), v.Org.ID, member.UserID); !errors.Is(err, orgs.ErrNotFound) {
		t.Fatalf("member still active: %v", err)
	}
	ended := 0
	for _, tp := range e.topics() {
		if tp == domainevent.TopicMembershipEnded {
			ended++
		}
	}
	if ended != 2 {
		t.Fatalf("membership.ended events = %d, want 2", ended)
	}
	_, err = e.svc.CloseOrg(t.Context(), pad(t), v.Org.ID, "Tekrar", proof(goodPassword))
	problemtest.WantProblem(t, err, 409, "orgs.invalid_state")
	// Without a proof: 403 identity.reauth_required, before any business data.
	v2, owner2 := e.company(t, "İkinci Şirket")
	_, err = e.svc.CloseOrg(t.Context(), owner2, v2.Org.ID, "Neden", proof(""))
	problemtest.WantProblem(t, err, 403, "identity.reauth_required")
}

func TestAdminVerifyOrg_PendingToActive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v, owner := e.company(t, "Doğrulanacak Şirket")
	_, err := e.svc.AdminVerifyOrg(t.Context(), owner, v.Org.ID, "")
	problemtest.WantErr(t, err, authz.ErrForbidden) // admin surface
	_, err = e.svc.AdminVerifyOrg(t.Context(), persona(t, "moderator"), v.Org.ID, "")
	problemtest.WantErr(t, err, authz.ErrForbidden)
	e.clk.Advance(time.Hour)
	admin := pad(t)
	got, err := e.svc.AdminVerifyOrg(t.Context(), admin, v.Org.ID, "Vergi levhası görüldü")
	if err != nil {
		t.Fatal(err)
	}
	o := got.Org
	if o.Status != orgs.StatusActive || o.VerifiedAt == nil || !o.VerifiedAt.Equal(t0.Add(time.Hour)) ||
		o.VerifiedBy == nil || *o.VerifiedBy != admin.UserID {
		t.Fatalf("verified org = %+v", o)
	}
	ev := e.audit.Of(authz.OrgsOrgVerify)
	if len(ev) != 1 || ev[0].Reason != "Vergi levhası görüldü" || ev[0].Metadata["from"] != "pending_review" {
		t.Fatalf("audit = %+v", ev)
	}
	// Public now; verifying again is 409; a missing org is 404 for the admin.
	if _, err := e.svc.GetOrg(t.Context(), authz.Anonymous(), v.Org.ID); err != nil {
		t.Fatalf("anonymous read after verification: %v", err)
	}
	_, err = e.svc.AdminVerifyOrg(t.Context(), admin, v.Org.ID, "")
	problemtest.WantProblem(t, err, 409, "orgs.invalid_state")
	_, err = e.svc.AdminVerifyOrg(t.Context(), admin, uuid.NewV7(), "")
	problemtest.WantErr(t, err, orgs.ErrNotFound)
}

// TestOrgWrites_EmitOrgUpdated: a profile edit and an admin status change
// write org.updated (O) for the web's revalidation; a failed write none
// (BE-106).
func TestOrgWrites_EmitOrgUpdated(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v, owner := e.company(t, "Yayın Şirketi")
	_, err := e.svc.AdminVerifyOrg(t.Context(), owner, v.Org.ID, "")
	problemtest.WantErr(t, err, authz.ErrForbidden)
	got, err := e.svc.AdminVerifyOrg(t.Context(), pad(t), v.Org.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	desc, size := "Yeni açıklama", "51-200"
	if _, err := e.svc.UpdateOrg(t.Context(), owner, v.Org.ID, got.ETag, orgs.OrgPatch{Description: &desc, SizeBand: &size}); err != nil {
		t.Fatal(err)
	}
	var evs []domainevent.Envelope
	for _, ev := range e.out.Events() {
		if ev.Topic == domainevent.TopicOrgUpdated {
			evs = append(evs, ev)
		}
	}
	if len(evs) != 2 {
		t.Fatalf("org.updated = %+v, want verify + update", evs)
	}
	for i, want := range [][]string{{"status"}, {"description", "size_band"}} {
		p, err := domainevent.Decode[domainevent.OrgUpdated](evs[i])
		if err != nil || p.OrgID != v.Org.ID || p.OrgKind != "company" || evs[i].AggregateID != v.Org.ID || !slices.Equal(p.Fields, want) {
			t.Fatalf("org.updated[%d] = %+v, %v; want fields %v", i, p, err, want)
		}
	}
}

func TestSuspendedOrg_WriteActions403_ReadAllowed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v, owner := e.company(t, "Askıya Alınan Şirket")
	admin := pad(t)
	ctx := t.Context()
	if _, err := e.svc.AdminVerifyOrg(ctx, admin, v.Org.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.AdminSuspendOrg(ctx, admin, v.Org.ID, "Şikâyet incelemesi", proof(""))
	problemtest.WantProblem(t, err, 403, "identity.reauth_required")
	s, err := e.svc.AdminSuspendOrg(ctx, admin, v.Org.ID, "Şikâyet incelemesi", proof(goodPassword))
	if err != nil || s.Org.Status != orgs.StatusSuspended {
		t.Fatalf("suspend = %+v, %v", s.Org, err)
	}
	name := "Yeni Ad"
	_, err = e.svc.UpdateOrg(ctx, owner, v.Org.ID, s.ETag, orgs.OrgPatch{Name: &name})
	problemtest.WantProblem(t, err, 403, "orgs.org_suspended")
	_, err = e.svc.CloseOrg(ctx, owner, v.Org.ID, "Kaçış", proof(goodPassword))
	problemtest.WantProblem(t, err, 403, "orgs.org_suspended")
	adminRow := e.member(t, v.Org.ID, authz.MemberCompanyAdmin)
	_, err = e.svc.TransferOwnership(ctx, owner, v.Org.ID, adminRow.UserID, proof(goodPassword))
	problemtest.WantProblem(t, err, 403, "orgs.org_suspended")
	// Reads stay allowed, for everyone.
	for _, a := range []authz.Actor{authz.Anonymous(), user(), owner} {
		if _, err := e.svc.GetOrg(ctx, a, v.Org.ID); err != nil {
			t.Fatalf("read of a suspended org by %s: %v", a, err)
		}
	}
	if _, err := e.svc.GetOrgBySlug(ctx, authz.Anonymous(), v.Org.Slug); err != nil {
		t.Fatalf("read by slug: %v", err)
	}
	// A platform admin may still correct the profile; suspending twice is 409.
	if _, err := e.svc.UpdateOrg(ctx, admin, v.Org.ID, s.ETag, orgs.OrgPatch{Name: &name}); err != nil {
		t.Fatalf("platform_admin update of a suspended org: %v", err)
	}
	_, err = e.svc.AdminSuspendOrg(ctx, admin, v.Org.ID, "Tekrar", proof(goodPassword))
	problemtest.WantProblem(t, err, 409, "orgs.invalid_state")
	r, err := e.svc.AdminReactivateOrg(ctx, admin, v.Org.ID, "İnceleme bitti", proof(goodPassword))
	if err != nil || r.Org.Status != orgs.StatusActive {
		t.Fatalf("reactivate = %+v, %v", r.Org, err)
	}
	if _, err := e.svc.UpdateOrg(ctx, owner, v.Org.ID, r.ETag, orgs.OrgPatch{Name: &name}); err != nil {
		t.Fatalf("owner update after reactivation: %v", err)
	}
	if n := len(e.audit.OfOutcome(authz.OrgsOrgSuspend, audit.Success)); n != 2 {
		t.Fatalf("suspend/reactivate audit records = %d", n)
	}
	if n := len(e.audit.OfOutcome(authz.OrgsOrgSuspend, audit.Denied)); n != 1 {
		t.Fatalf("%d denied records for the missing proof, want 1 (03 §6.2, BE-074)", n)
	}
	// The platform org is never suspended.
	platform, _, err := e.store.InsertOrg(ctx, orgs.Org{ID: uuid.NewV7(), Kind: orgs.KindPlatform, Name: "Koro", Slug: "koro", Status: orgs.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AdminSuspendOrg(ctx, admin, platform.ID, "Deneme", proof(goodPassword))
	problemtest.WantProblem(t, err, 409, "orgs.invalid_state")
}

func TestOrgAdminCreate_JoinsCallerTx(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tx := &recordingStore{MemStore: e.store}
	mod, err := orgs.New(orgs.Deps{
		Store: tx, Tx: e.tx, Outbox: e.out, Audit: e.audit, Clock: e.clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: e.tx, Store: idempotency.NewMemStore(), Clock: e.clk, Log: obs.Discard()}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	var id uuid.UUID
	err = e.tx.WithTx(ctx, func(ctx context.Context) error {
		caller := dbtest.TxOf(ctx)
		var err error
		id, err = mod.Service.Create(ctx, orgs.CreateOrg{Kind: orgs.KindUniversity, Name: "Ege Üniversitesi", City: "İzmir"})
		if err != nil {
			return err
		}
		if !slices.Equal(tx.txs(), []int{caller}) {
			t.Errorf("Create wrote in transactions %v, want the caller's %v", tx.txs(), caller)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.tx.Count() != 1 {
		t.Fatalf("Create opened its own transaction (%d transactions)", e.tx.Count())
	}
	s, err := mod.Service.Get(ctx, id)
	if err != nil || s.Kind != orgs.KindUniversity || s.Status != orgs.StatusActive || s.Slug != "ege-universitesi" {
		t.Fatalf("Get = %+v, %v", s, err)
	}
	many, err := mod.Service.GetMany(ctx, []uuid.UUID{id, uuid.NewV7()})
	if err != nil || len(many) != 1 || many[id].Name != "Ege Üniversitesi" {
		t.Fatalf("GetMany = %+v, %v", many, err)
	}
	// Outside a transaction it opens one; a company gets its profile row.
	cid, err := mod.Service.Create(ctx, orgs.CreateOrg{Kind: orgs.KindCompany, Name: "İçe Aktarılan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.GetCompany(ctx, cid); err != nil {
		t.Fatalf("company profile of an imported company: %v", err)
	}
	// Kinds it does not create.
	for _, k := range []orgs.Kind{orgs.KindPlatform, orgs.KindNGO} {
		if _, err := mod.Service.Create(ctx, orgs.CreateOrg{Kind: k, Name: "Olmaz"}); !errors.Is(err, orgs.ErrInvalidState) {
			t.Fatalf("Create(%s) = %v", k, err)
		}
	}
}

// recordingStore remembers the transaction of every InsertOrg.
type recordingStore struct {
	*orgs.MemStore
	seen []int
}

func (r *recordingStore) InsertOrg(ctx context.Context, o orgs.Org) (orgs.Org, bool, error) {
	r.seen = append(r.seen, dbtest.TxOf(ctx))
	return r.MemStore.InsertOrg(ctx, o)
}

func (r *recordingStore) txs() []int { return r.seen }

// TestOrgSummary_LogoAndVerified (BE-094, T-251): the summary other
// modules read carries the logo file and the verification badge.
func TestOrgSummary_LogoAndVerified(t *testing.T) {
	t.Parallel()
	logo, at := uuid.NewV7(), time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s := orgs.Org{ID: uuid.NewV7(), Kind: orgs.KindClub, LogoFileID: &logo, VerifiedAt: &at}.Summary()
	if s.LogoFileID == nil || *s.LogoFileID != logo || !s.Verified {
		t.Fatalf("summary = %+v", s)
	}
	if s := (orgs.Org{ID: uuid.NewV7()}).Summary(); s.LogoFileID != nil || s.Verified {
		t.Fatalf("summary without logo/verification = %+v", s)
	}
}
