package orgs_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// world creates the parent rows a store needs (users: FKs in PostgreSQL;
// plain IDs in memory).
type world struct {
	user func(t *testing.T) uuid.UUID
}

func memWorld() world { return world{user: func(*testing.T) uuid.UUID { return uuid.NewV7() }} }

var slugSeq atomic.Int64

func newOrg(kind orgs.Kind, name string) orgs.Org {
	return orgs.Org{ID: uuid.NewV7(), Kind: kind, Name: name, Slug: "org-" + strconv.FormatInt(slugSeq.Add(1), 10),
		Status: orgs.StatusActive}
}

// storeContract is the contract both stores pass (05 §13.3).
func storeContract(t *testing.T, newStore func(t *testing.T) (orgs.Store, world)) {
	t.Helper()
	now := t0
	insert := func(t *testing.T, s orgs.Store, o orgs.Org) orgs.Org {
		t.Helper()
		got, ok, err := s.InsertOrg(t.Context(), o)
		if err != nil || !ok {
			t.Fatalf("InsertOrg: ok=%v err=%v", ok, err)
		}
		return got
	}
	member := func(org, user uuid.UUID, role authz.MemberRole) orgs.Membership {
		return orgs.Membership{ID: uuid.NewV7(), OrgID: org, UserID: user, Role: role, Status: orgs.MemberActive,
			Source: orgs.SourceAdmin, JoinedAt: &now}
	}

	t.Run("orgs: insert, read, slug, update", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		by := w.user(t)
		o := newOrg(orgs.KindCompany, "Acme")
		o.CreatedBy, o.City = &by, new("İstanbul")
		got := insert(t, s, o)
		if got.ID != o.ID || got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() || *got.CreatedBy != by {
			t.Fatalf("stored = %+v", got)
		}
		if g, err := s.GetOrg(ctx, o.ID); err != nil || g.Slug != o.Slug || *g.City != "İstanbul" {
			t.Fatalf("GetOrg = %+v, %v", g, err)
		}
		if g, err := s.GetOrgBySlug(ctx, o.Slug); err != nil || g.ID != o.ID {
			t.Fatalf("GetOrgBySlug = %+v, %v", g, err)
		}
		if _, err := s.GetOrg(ctx, uuid.NewV7()); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("GetOrg missing: %v", err)
		}
		if _, err := s.LockOrg(ctx, uuid.NewV7()); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("LockOrg missing: %v", err)
		}
		// A taken slug writes nothing and reports false.
		dup := newOrg(orgs.KindClub, "Başka")
		dup.Slug = o.Slug
		if _, ok, err := s.InsertOrg(ctx, dup); err != nil || ok {
			t.Fatalf("InsertOrg with a taken slug: ok=%v err=%v", ok, err)
		}
		// CHECK constraints: bad slug, closed without closed_at → 422.
		bad := newOrg(orgs.KindClub, "Kötü")
		bad.Slug = "Kötü Slug"
		if _, _, err := s.InsertOrg(ctx, bad); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("InsertOrg with a bad slug: %v", err)
		}
		closed := got
		closed.Status = orgs.StatusClosed
		if _, err := s.UpdateOrg(ctx, closed); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("closed without closed_at: %v", err)
		}
		upd := got
		upd.Name, upd.Status, upd.VerifiedAt, upd.VerifiedBy = "Acme Yazılım", orgs.StatusActive, &now, &by
		after, err := s.UpdateOrg(ctx, upd)
		if err != nil || after.Name != "Acme Yazılım" || after.VerifiedBy == nil || !after.UpdatedAt.After(got.UpdatedAt) {
			t.Fatalf("UpdateOrg = %+v (before %s), %v", after, got.UpdatedAt, err)
		}
		// A rejection reason (BE-144) belongs to a closed org only.
		open := after
		open.RejectionReason = new("Belgeler eksik")
		if _, err := s.UpdateOrg(ctx, open); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("rejection reason on an open org: %v", err)
		}
		rej := after
		rej.Status, rej.ClosedAt, rej.RejectionReason = orgs.StatusClosed, &now, new("x")
		if _, err := s.UpdateOrg(ctx, rej); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("a 1-character rejection reason: %v", err)
		}
		rej.RejectionReason = new("Vergi levhası doğrulanamadı")
		if r, err := s.UpdateOrg(ctx, rej); err != nil || r.RejectionReason == nil || *r.RejectionReason != "Vergi levhası doğrulanamadı" {
			t.Fatalf("UpdateOrg with a rejection reason = %+v, %v", r, err)
		}
		if g, err := s.GetOrg(ctx, o.ID); err != nil || g.RejectionReason == nil || *g.RejectionReason != "Vergi levhası doğrulanamadı" {
			t.Fatalf("GetOrg rejection reason = %+v, %v", g.RejectionReason, err)
		}
		gone := newOrg(orgs.KindClub, "Yok")
		if _, err := s.UpdateOrg(ctx, gone); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("UpdateOrg missing: %v", err)
		}
		many, err := s.GetOrgs(ctx, []uuid.UUID{o.ID, uuid.NewV7()})
		if err != nil || len(many) != 1 || many[0].ID != o.ID {
			t.Fatalf("GetOrgs = %+v, %v", many, err)
		}
		created, err := s.OrgsCreatedBy(ctx, by)
		if err != nil || len(created) != 1 || created[0].ID != o.ID {
			t.Fatalf("OrgsCreatedBy = %+v, %v", created, err)
		}
	})

	t.Run("companies: parent kind and scope", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		ctx := t.Context()
		co := insert(t, s, newOrg(orgs.KindCompany, "Şirket"))
		club := insert(t, s, newOrg(orgs.KindClub, "Kulüp"))
		sector := "software"
		if err := s.InsertCompany(ctx, orgs.Company{OrgID: co.ID, OrgKind: orgs.KindClub, SectorCode: &sector}); err != nil {
			t.Fatalf("InsertCompany: %v", err)
		}
		c, err := s.GetCompany(ctx, co.ID)
		if err != nil || c.OrgKind != orgs.KindCompany || *c.SectorCode != "software" {
			t.Fatalf("GetCompany = %+v, %v (org_kind comes from the parent)", c, err)
		}
		// 06 H-38: a club parent fails the companies CHECK, whatever the caller says.
		if err := s.InsertCompany(ctx, orgs.Company{OrgID: club.ID, OrgKind: orgs.KindCompany}); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("company profile of a club: %v", err)
		}
		if err := s.InsertCompany(ctx, orgs.Company{OrgID: uuid.NewV7()}); err == nil {
			t.Fatal("company profile of a missing org")
		}
		if _, err := s.GetCompany(ctx, uuid.Nil()); !errors.Is(err, db.ErrMissingOrgScope) {
			t.Fatalf("GetCompany without org: %v", err)
		}
		if _, err := s.GetCompany(ctx, club.ID); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("GetCompany of a club: %v", err)
		}
		size, domain := "1000+", "acme.com.tr"
		c.SizeBand, c.EmailDomain = &size, &domain
		got, err := s.UpdateCompany(ctx, c)
		if err != nil || *got.SizeBand != "1000+" || *got.EmailDomain != "acme.com.tr" {
			t.Fatalf("UpdateCompany = %+v, %v", got, err)
		}
		badSize := "çok"
		c.SizeBand = &badSize
		if _, err := s.UpdateCompany(ctx, c); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("UpdateCompany with a bad size band: %v", err)
		}
	})

	t.Run("companies: tax identity, domain verification, CompaniesOf (BE-165)", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		ctx := t.Context()
		co := insert(t, s, newOrg(orgs.KindCompany, "Vergili Şirket"))
		other := insert(t, s, newOrg(orgs.KindCompany, "Profilsiz Şirket"))
		club := insert(t, s, newOrg(orgs.KindClub, "Vergisiz Kulüp"))
		vkn, tckn := orgs.TaxIDVKN, orgs.TaxIDTCKN
		no, office, domain := "1234567890", "Kadıköy", "vergili.example.test"
		verified := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
		if err := s.InsertCompany(ctx, orgs.Company{OrgID: co.ID, TaxIDKind: &vkn, TaxNo: &no, TaxOffice: &office,
			EmailDomain: &domain, DomainVerifiedAt: &verified}); err != nil {
			t.Fatalf("InsertCompany: %v", err)
		}
		c, err := s.GetCompany(ctx, co.ID)
		if err != nil || *c.TaxIDKind != vkn || *c.TaxNo != no || *c.TaxOffice != office || !c.DomainVerifiedAt.Equal(verified) ||
			!c.TaxIDValid() || !c.DomainVerified() {
			t.Fatalf("GetCompany = %+v, %v", c, err)
		}
		// The CHECKs: a number belongs to kind vkn, kind vkn has one, the office is trimmed.
		for name, bad := range map[string]func(c orgs.Company) orgs.Company{
			"kind tckn with a number": func(c orgs.Company) orgs.Company { c.TaxIDKind = &tckn; return c },
			"kind vkn without one":    func(c orgs.Company) orgs.Company { c.TaxNo = nil; return c },
			"an 11-digit number":      func(c orgs.Company) orgs.Company { n := "10000000146"; c.TaxNo = &n; return c },
			"an untrimmed office":     func(c orgs.Company) orgs.Company { o := " Kadıköy"; c.TaxOffice = &o; return c },
			"an unknown kind": func(c orgs.Company) orgs.Company {
				k := orgs.TaxIDKind("mersis")
				c.TaxIDKind, c.TaxNo = &k, nil
				return c
			},
		} {
			if _, err := s.UpdateCompany(ctx, bad(c)); !errors.Is(err, problem.ErrValidation) {
				t.Errorf("UpdateCompany with %s: %v", name, err)
			}
		}
		// A TCKN is kept as its kind only; the verification is cleared.
		c.TaxIDKind, c.TaxNo, c.DomainVerifiedAt = &tckn, nil, nil
		got, err := s.UpdateCompany(ctx, c)
		if err != nil || *got.TaxIDKind != tckn || got.TaxNo != nil || got.DomainVerifiedAt != nil || got.DomainVerified() {
			t.Fatalf("UpdateCompany = %+v, %v", got, err)
		}
		if err := s.InsertCompany(ctx, orgs.Company{OrgID: other.ID}); err != nil {
			t.Fatal(err)
		}
		all, err := s.CompaniesOf(ctx, []uuid.UUID{co.ID, other.ID, club.ID, uuid.NewV7()})
		if err != nil || len(all) != 2 || all[co.ID].TaxIDKind == nil || all[other.ID].TaxIDKind != nil {
			t.Fatalf("CompaniesOf = %+v, %v", all, err)
		}
		if none, err := s.CompaniesOf(ctx, nil); err != nil || len(none) != 0 {
			t.Fatalf("CompaniesOf(nil) = %v, %v", none, err)
		}
	})

	t.Run("members: parent kind, uniqueness, end", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		co := insert(t, s, newOrg(orgs.KindCompany, "Şirket"))
		other := insert(t, s, newOrg(orgs.KindCompany, "Diğer"))
		club := insert(t, s, newOrg(orgs.KindClub, "Kulüp"))
		owner, admin, rec := w.user(t), w.user(t), w.user(t)

		m := member(co.ID, owner, authz.MemberCompanyOwner)
		m.OrgKind = orgs.KindClub // ignored: from the parent row
		got, err := s.InsertMember(ctx, m)
		if err != nil || got.OrgKind != orgs.KindCompany || got.CreatedAt.IsZero() {
			t.Fatalf("InsertMember = %+v, %v", got, err)
		}
		// A company role in a club org fails the role/kind CHECK.
		if _, err := s.InsertMember(ctx, member(club.ID, admin, authz.MemberCompanyAdmin)); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("company role in a club: %v", err)
		}
		if _, err := s.InsertMember(ctx, member(uuid.NewV7(), admin, authz.MemberCompanyAdmin)); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("membership of a missing org: %v", err)
		}
		// One open membership per user and org; one active owner per company.
		if _, err := s.InsertMember(ctx, member(co.ID, owner, authz.MemberCompanyAdmin)); !errors.Is(err, problem.ErrConflict) {
			t.Fatalf("second open membership: %v", err)
		}
		if _, err := s.InsertMember(ctx, member(co.ID, admin, authz.MemberCompanyOwner)); !errors.Is(err, problem.ErrConflict) {
			t.Fatalf("second owner: %v", err)
		}
		a, err := s.InsertMember(ctx, member(co.ID, admin, authz.MemberCompanyAdmin))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.InsertMember(ctx, member(co.ID, rec, authz.MemberRecruiter)); err != nil {
			t.Fatal(err)
		}
		if o, err := s.LockOwner(ctx, co.ID); err != nil || o.UserID != owner {
			t.Fatalf("LockOwner = %+v, %v", o, err)
		}
		if x, err := s.LockActiveMember(ctx, co.ID, admin); err != nil || x.ID != a.ID {
			t.Fatalf("LockActiveMember = %+v, %v", x, err)
		}
		if _, err := s.LockActiveMember(ctx, other.ID, admin); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("LockActiveMember in another org: %v", err)
		}
		if _, err := s.LockOwner(ctx, other.ID); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("LockOwner of an ownerless org: %v", err)
		}
		// Org scope: another org's membership cannot be ended through this org.
		if _, err := s.EndMember(ctx, other.ID, a.ID, now, orgs.EndRoleChanged); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("EndMember under another org: %v", err)
		}
		e, err := s.EndMember(ctx, co.ID, a.ID, now, orgs.EndRoleChanged)
		if err != nil || e.Status != orgs.MemberEnded || *e.EndReason != orgs.EndRoleChanged || e.EndedAt == nil {
			t.Fatalf("EndMember = %+v, %v", e, err)
		}
		if _, err := s.EndMember(ctx, co.ID, a.ID, now, orgs.EndRoleChanged); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("EndMember twice: %v", err)
		}
		// EndedMemberships (BE-090): the ended row only; an active row and
		// an unknown ID are missing.
		if ids, err := s.EndedMemberships(ctx, []uuid.UUID{got.ID, a.ID, uuid.NewV7()}); err != nil || !slices.Equal(ids, []uuid.UUID{a.ID}) {
			t.Fatalf("EndedMemberships = %v, %v; want [%s]", ids, err, a.ID)
		}
		if ids, err := s.EndedMemberships(ctx, nil); err != nil || len(ids) != 0 {
			t.Fatalf("EndedMemberships(nil) = %v, %v", ids, err)
		}
		// The user can join again after the end (open uniqueness only).
		if _, err := s.InsertMember(ctx, member(co.ID, admin, authz.MemberCompanyOwner)); !errors.Is(err, problem.ErrConflict) {
			t.Fatalf("owner while another is active: %v", err)
		}
		if _, err := s.EndOpenMembers(ctx, uuid.Nil(), now, orgs.EndOrgClosed); !errors.Is(err, db.ErrMissingOrgScope) {
			t.Fatalf("EndOpenMembers without org: %v", err)
		}
		gone, err := s.EndOpenMembers(ctx, co.ID, now, orgs.EndOrgClosed)
		if err != nil || len(gone) != 2 || slices.ContainsFunc(gone, func(m orgs.Membership) bool { return m.Status != orgs.MemberActive }) {
			t.Fatalf("EndOpenMembers = %+v, %v", gone, err)
		}
		if _, err := s.LockOwner(ctx, co.ID); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("owner after closing: %v", err)
		}
		rows, err := s.MembershipsOfUser(ctx, admin)
		if err != nil || len(rows) != 1 || rows[0].Status != orgs.MemberEnded {
			t.Fatalf("MembershipsOfUser = %+v, %v", rows, err)
		}
	})

	t.Run("members: retention purge of ended memberships (BE-057)", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		club := insert(t, s, newOrg(orgs.KindClub, "Saklama Kulübü"))
		var ended []uuid.UUID
		for i, at := range []time.Time{now.AddDate(0, -8, 0), now.AddDate(0, -7, 0), now} {
			m, err := s.InsertMember(ctx, member(club.ID, w.user(t), authz.MemberMember))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.EndMember(ctx, club.ID, m.ID, at, orgs.EndLeft); err != nil {
				t.Fatalf("end %d: %v", i, err)
			}
			ended = append(ended, m.ID)
		}
		active, err := s.InsertMember(ctx, member(club.ID, w.user(t), authz.MemberMember))
		if err != nil {
			t.Fatal(err)
		}
		cutoff := now.AddDate(0, -6, 0)
		if n, err := s.PurgeExpired(ctx, orgs.PolicyMembershipEnded, cutoff, 0, true); err != nil || n != 2 {
			t.Fatalf("dry run = %d, %v; want 2", n, err)
		}
		if n, err := s.PurgeExpired(ctx, orgs.PolicyMembershipEnded, cutoff, 1, false); err != nil || n != 1 {
			t.Fatalf("batch of 1 = %d, %v", n, err)
		}
		if _, err := s.GetMember(ctx, club.ID, ended[0]); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("the oldest ended membership survived the first batch: %v", err)
		}
		if n, err := s.PurgeExpired(ctx, orgs.PolicyMembershipEnded, cutoff, 10, false); err != nil || n != 1 {
			t.Fatalf("second batch = %d, %v", n, err)
		}
		for _, id := range []uuid.UUID{ended[2], active.ID} {
			if _, err := s.GetMember(ctx, club.ID, id); err != nil {
				t.Fatalf("a young or active membership was purged: %v", err)
			}
		}
	})

	t.Run("members: lifecycle queries (BE-019)", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		club := insert(t, s, newOrg(orgs.KindClub, "Kulüp"))
		other := insert(t, s, newOrg(orgs.KindClub, "Öteki"))
		u1, u2, u3, approver := w.user(t), w.user(t), w.user(t), w.user(t)
		req := now.Add(-time.Hour)
		pending, err := s.InsertMember(ctx, orgs.Membership{ID: uuid.NewV7(), OrgID: club.ID, UserID: u1, Role: authz.MemberMember,
			Status: orgs.MemberPending, Source: orgs.SourceJoinRequest, RequestedAt: &req})
		if err != nil {
			t.Fatal(err)
		}
		early := now.Add(-48 * time.Hour)
		admin := member(club.ID, u2, authz.MemberClubAdmin)
		admin.JoinedAt = &early
		admin, err = s.InsertMember(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := s.InsertMember(ctx, member(club.ID, u3, authz.MemberMember))
		if err != nil {
			t.Fatal(err)
		}
		// Scoped reads: another org's ID is not found (06 H-38).
		if m, err := s.GetMember(ctx, club.ID, pending.ID); err != nil || m.Status != orgs.MemberPending {
			t.Fatalf("GetMember = %+v, %v", m, err)
		}
		for _, f := range []func() error{
			func() error { _, err := s.GetMember(ctx, other.ID, pending.ID); return err },
			func() error { _, err := s.LockMember(ctx, other.ID, pending.ID); return err },
			func() error { _, err := s.LockMemberOfUser(ctx, u2, pending.ID); return err },
			func() error { _, err := s.LockOpenMember(ctx, other.ID, u1); return err },
			func() error { _, err := s.ActivateMember(ctx, other.ID, pending.ID, now, approver); return err },
		} {
			if err := f(); !errors.Is(err, orgs.ErrNotFound) {
				t.Fatalf("cross-org access: %v", err)
			}
		}
		if m, err := s.LockMemberOfUser(ctx, u1, pending.ID); err != nil || m.ID != pending.ID {
			t.Fatalf("LockMemberOfUser = %+v, %v", m, err)
		}
		if m, err := s.LockOpenMember(ctx, club.ID, u1); err != nil || m.ID != pending.ID {
			t.Fatalf("LockOpenMember = %+v, %v", m, err)
		}
		// Listing: open rows by default, filters, both orders with keyset.
		ids := func(ms []orgs.Membership) []uuid.UUID {
			var out []uuid.UUID
			for _, m := range ms {
				out = append(out, m.ID)
			}
			return out
		}
		rows, err := s.ListMembers(ctx, club.ID, orgs.MemberFilter{}, nil, 10)
		if err != nil || !slices.Equal(ids(rows), []uuid.UUID{plain.ID, admin.ID, pending.ID}) {
			t.Fatalf("ListMembers newest = %v, %v", ids(rows), err)
		}
		rows, err = s.ListMembers(ctx, club.ID, orgs.MemberFilter{Sort: orgs.SortMemberJoined}, nil, 2)
		if err != nil || !slices.Equal(ids(rows), []uuid.UUID{admin.ID, pending.ID}) {
			t.Fatalf("ListMembers by join date = %v, %v", ids(rows), err)
		}
		rows, err = s.ListMembers(ctx, club.ID, orgs.MemberFilter{Sort: orgs.SortMemberJoined},
			&orgs.MemberAfter{Since: rows[1].Since(), ID: rows[1].ID}, 2)
		if err != nil || !slices.Equal(ids(rows), []uuid.UUID{plain.ID}) {
			t.Fatalf("ListMembers by join date, page 2 = %v, %v", ids(rows), err)
		}
		rows, err = s.ListMembers(ctx, club.ID, orgs.MemberFilter{}, &orgs.MemberAfter{ID: plain.ID}, 1)
		if err != nil || !slices.Equal(ids(rows), []uuid.UUID{admin.ID}) {
			t.Fatalf("ListMembers newest, page 2 = %v, %v", ids(rows), err)
		}
		rows, err = s.ListMembers(ctx, club.ID, orgs.MemberFilter{Statuses: []orgs.MemberStatus{orgs.MemberPending}}, nil, 10)
		if err != nil || !slices.Equal(ids(rows), []uuid.UUID{pending.ID}) {
			t.Fatalf("ListMembers pending = %v, %v", ids(rows), err)
		}
		rows, err = s.ListMembers(ctx, club.ID, orgs.MemberFilter{Role: authz.MemberClubAdmin}, nil, 10)
		if err != nil || !slices.Equal(ids(rows), []uuid.UUID{admin.ID}) {
			t.Fatalf("ListMembers club_admin = %v, %v", ids(rows), err)
		}
		// Approval: pending → active once.
		act, err := s.ActivateMember(ctx, club.ID, pending.ID, now, approver)
		if err != nil || act.Status != orgs.MemberActive || act.JoinedAt == nil || *act.ApprovedBy != approver {
			t.Fatalf("ActivateMember = %+v, %v", act, err)
		}
		if _, err := s.ActivateMember(ctx, club.ID, pending.ID, now, approver); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("ActivateMember twice: %v", err)
		}
		if ms, err := s.LockActiveByRoles(ctx, club.ID, []authz.MemberRole{authz.MemberClubAdmin}); err != nil || !slices.Equal(ids(ms), []uuid.UUID{admin.ID}) {
			t.Fatalf("LockActiveByRoles = %v, %v", ids(ms), err)
		}
		if ms, err := s.LockUserActiveByRoles(ctx, u2, []authz.MemberRole{authz.MemberClubAdmin, authz.MemberMember}); err != nil || len(ms) != 1 {
			t.Fatalf("LockUserActiveByRoles = %v, %v", ids(ms), err)
		}
		if ms, err := s.ActiveMembershipsOfUser(ctx, u1); err != nil || !slices.Equal(ids(ms), []uuid.UUID{pending.ID}) {
			t.Fatalf("ActiveMembershipsOfUser = %v, %v", ids(ms), err)
		}
		want := []uuid.UUID{u1, u2, u3}
		slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		if got, err := s.ActiveMemberIDs(ctx, club.ID); err != nil || !slices.Equal(got, want) {
			t.Fatalf("ActiveMemberIDs = %v, %v", got, err)
		}
		if ms, err := s.ActiveByRole(ctx, authz.MemberClubAdmin, uuid.Nil(), 10); err != nil || !slices.Equal(ids(ms), []uuid.UUID{admin.ID}) {
			t.Fatalf("ActiveByRole = %v, %v", ids(ms), err)
		}
		if ms, err := s.ActiveByRole(ctx, authz.MemberClubAdmin, admin.ID, 10); err != nil || len(ms) != 0 {
			t.Fatalf("ActiveByRole after the last = %v, %v", ids(ms), err)
		}
		// The user's own list: newest first, by status.
		if _, err := s.EndMember(ctx, club.ID, plain.ID, now, orgs.EndLeft); err != nil {
			t.Fatal(err)
		}
		if ms, err := s.ListUserMemberships(ctx, u3, nil, nil, 10); err != nil || len(ms) != 0 {
			t.Fatalf("ListUserMemberships open = %v, %v", ids(ms), err)
		}
		if ms, err := s.ListUserMemberships(ctx, u3, []orgs.MemberStatus{orgs.MemberEnded}, nil, 10); err != nil || !slices.Equal(ids(ms), []uuid.UUID{plain.ID}) {
			t.Fatalf("ListUserMemberships ended = %v, %v", ids(ms), err)
		}
		// Ended rows of a club that became sensitive: scoped bulk delete.
		if n, err := s.DeleteEndedMembers(ctx, other.ID, 10); err != nil || n != 0 {
			t.Fatalf("DeleteEndedMembers of another org = %d, %v", n, err)
		}
		// Hard delete (sensitive club), idempotent and scoped.
		if err := s.DeleteMember(ctx, other.ID, plain.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetMember(ctx, club.ID, plain.ID); err != nil {
			t.Fatalf("DeleteMember under another org removed the row: %v", err)
		}
		for range 2 {
			if err := s.DeleteMember(ctx, club.ID, plain.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.GetMember(ctx, club.ID, plain.ID); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("deleted membership found: %v", err)
		}
		if _, err := s.EndMember(ctx, club.ID, admin.ID, now, orgs.EndRemoved); err != nil {
			t.Fatal(err)
		}
		if n, err := s.DeleteEndedMembers(ctx, club.ID, 10); err != nil || n != 1 {
			t.Fatalf("DeleteEndedMembers = %d, %v", n, err)
		}
		if m, err := s.GetMember(ctx, club.ID, pending.ID); err != nil || m.Status != orgs.MemberActive {
			t.Fatalf("an open row was deleted: %+v, %v", m, err)
		}
	})

	t.Run("invitations: parent kind, open uniqueness, lifecycle", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		co := insert(t, s, newOrg(orgs.KindCompany, "Davetli Şirket"))
		other := insert(t, s, newOrg(orgs.KindCompany, "Başka Şirket"))
		inviter, acceptor := w.user(t), w.user(t)
		hash := func(b byte) []byte { return slices.Repeat([]byte{b}, 32) }
		inv := orgs.Invitation{ID: uuid.NewV7(), OrgID: co.ID, OrgKind: orgs.KindClub, Email: "Ayse@Example.com",
			EmailNormalized: "ayse@example.com", Role: authz.MemberRecruiter, KeyID: "k1", TokenHash: hash(1),
			InvitedBy: inviter, ExpiresAt: now.Add(7 * 24 * time.Hour)}
		got, err := s.InsertInvitation(ctx, inv)
		if err != nil || got.OrgKind != orgs.KindCompany || got.CreatedAt.IsZero() {
			t.Fatalf("InsertInvitation = %+v, %v", got, err)
		}
		dup := inv
		dup.ID, dup.TokenHash = uuid.NewV7(), hash(2)
		if _, err := s.InsertInvitation(ctx, dup); !errors.Is(err, orgs.ErrInvitationExists) {
			t.Fatalf("second open invitation: %v", err)
		}
		bad := dup
		bad.EmailNormalized, bad.Role = "b@example.com", authz.MemberCompanyOwner
		if _, err := s.InsertInvitation(ctx, bad); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("company_owner invitation: %v", err)
		}
		bad.Role, bad.OrgID = authz.MemberRecruiter, uuid.NewV7()
		if _, err := s.InsertInvitation(ctx, bad); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("invitation of a missing org: %v", err)
		}
		if _, err := s.GetInvitation(ctx, other.ID, inv.ID); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("GetInvitation under another org: %v", err)
		}
		if x, err := s.LockInvitationByTokenHash(ctx, hash(1)); err != nil || x.ID != inv.ID {
			t.Fatalf("LockInvitationByTokenHash = %+v, %v", x, err)
		}
		if _, err := s.LockInvitationByTokenHash(ctx, hash(9)); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("unknown token: %v", err)
		}
		if x, err := s.LockOpenInvitationTo(ctx, co.ID, "ayse@example.com"); err != nil || x.ID != inv.ID {
			t.Fatalf("LockOpenInvitationTo = %+v, %v", x, err)
		}
		if rows, err := s.ListOpenInvitations(ctx, co.ID, now, nil, 10); err != nil || len(rows) != 1 {
			t.Fatalf("ListOpenInvitations = %d, %v", len(rows), err)
		}
		if rows, err := s.ListOpenInvitations(ctx, co.ID, inv.ExpiresAt, nil, 10); err != nil || len(rows) != 0 {
			t.Fatalf("ListOpenInvitations at expiry = %d, %v", len(rows), err)
		}
		if _, err := s.AcceptInvitation(ctx, other.ID, inv.ID, acceptor, now); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("AcceptInvitation under another org: %v", err)
		}
		acc, err := s.AcceptInvitation(ctx, co.ID, inv.ID, acceptor, now)
		if err != nil || acc.AcceptedAt == nil || *acc.AcceptedUserID != acceptor {
			t.Fatalf("AcceptInvitation = %+v, %v", acc, err)
		}
		if _, err := s.AcceptInvitation(ctx, co.ID, inv.ID, acceptor, now); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("accept twice: %v", err)
		}
		if _, err := s.CloseInvitation(ctx, co.ID, inv.ID, now); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("revoke an accepted invitation: %v", err)
		}
		// The address is free again; an expired invitation is closed by the job.
		old := dup
		old.ExpiresAt = now.Add(-time.Hour)
		if _, err := s.InsertInvitation(ctx, old); err != nil {
			t.Fatal(err)
		}
		if n, err := s.CloseExpiredInvitations(ctx, now, 10); err != nil || n != 1 {
			t.Fatalf("CloseExpiredInvitations = %d, %v", n, err)
		}
		if x, err := s.GetInvitation(ctx, co.ID, old.ID); err != nil || x.RevokedAt == nil || !x.RevokedAt.Equal(old.ExpiresAt) {
			t.Fatalf("closed = %+v, %v", x, err)
		}
		if n, err := s.CloseExpiredInvitations(ctx, now, 10); err != nil || n != 0 {
			t.Fatalf("CloseExpiredInvitations again = %d, %v", n, err)
		}
		if rows, err := s.InvitationsOfUser(ctx, acceptor, ""); err != nil || len(rows) != 1 {
			t.Fatalf("InvitationsOfUser = %d, %v", len(rows), err)
		}
		if rows, err := s.InvitationsOfUser(ctx, acceptor, "ayse@example.com"); err != nil || len(rows) != 2 {
			t.Fatalf("InvitationsOfUser with the address = %d, %v", len(rows), err)
		}
		// Retention: closed before the cutoff are deleted; an open one stays.
		open := orgs.Invitation{ID: uuid.NewV7(), OrgID: co.ID, Email: "c@example.com", EmailNormalized: "c@example.com",
			Role: authz.MemberCompanyAdmin, KeyID: "k1", TokenHash: hash(3), InvitedBy: inviter, ExpiresAt: now.Add(time.Hour)}
		if _, err := s.InsertInvitation(ctx, open); err != nil {
			t.Fatal(err)
		}
		if n, err := s.PurgeExpired(ctx, orgs.PolicyOrgInvitation, now.Add(-2*time.Hour), 10, false); err != nil || n != 0 {
			t.Fatalf("PurgeExpired before the cutoff = %d, %v", n, err)
		}
		if n, err := s.PurgeExpired(ctx, orgs.PolicyOrgInvitation, now.Add(time.Minute), 10, true); err != nil || n != 2 {
			t.Fatalf("PurgeExpired dry run = %d, %v", n, err)
		}
		if n, err := s.PurgeExpired(ctx, orgs.PolicyOrgInvitation, now.Add(time.Minute), 1, false); err != nil || n != 1 {
			t.Fatalf("PurgeExpired batch of 1 = %d, %v", n, err)
		}
		if n, err := s.PurgeExpired(ctx, orgs.PolicyOrgInvitation, now.Add(time.Minute), 10, false); err != nil || n != 1 {
			t.Fatalf("PurgeExpired = %d, %v", n, err)
		}
		if _, err := s.PurgeExpired(ctx, "no_such_policy", now, 10, true); err == nil {
			t.Fatal("PurgeExpired(unknown policy) succeeded")
		}
		if n, err := s.DeleteInvitationsOfUser(ctx, acceptor, "c@example.com"); err != nil || n != 1 {
			t.Fatalf("DeleteInvitationsOfUser = %d, %v", n, err)
		}
		if _, err := s.GetInvitation(ctx, co.ID, open.ID); !errors.Is(err, orgs.ErrNotFound) {
			t.Fatalf("erased invitation found: %v", err)
		}
	})

	t.Run("list: filters, sort and keyset", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		ctx := t.Context()
		tag := "z" + strconv.FormatInt(slugSeq.Add(1), 10) // isolates this test's rows in a shared database
		var ids []uuid.UUID
		for _, x := range []struct {
			name   string
			kind   orgs.Kind
			status orgs.Status
		}{
			{"Alfa Kulübü " + tag, orgs.KindClub, orgs.StatusActive},
			{"Beta Şirketi " + tag, orgs.KindCompany, orgs.StatusActive},
			{"Gama Şirketi " + tag, orgs.KindCompany, orgs.StatusSuspended},
			{"Delta Şirketi " + tag, orgs.KindCompany, orgs.StatusPendingReview},
		} {
			o := newOrg(x.kind, x.name)
			o.Status = x.status
			ids = append(ids, insert(t, s, o).ID)
		}
		names := func(rows []orgs.Org) []string {
			var out []string
			for _, o := range rows {
				out = append(out, o.Name[:4])
			}
			return out
		}
		pub := []orgs.Status{orgs.StatusActive, orgs.StatusSuspended}
		rows, err := s.ListOrgs(ctx, orgs.ListFilter{Statuses: pub, Q: tag, Sort: orgs.SortName}, nil, 10)
		if err != nil || !slices.Equal(names(rows), []string{"Alfa", "Beta", "Gama"}) {
			t.Fatalf("public by name = %v, %v", names(rows), err)
		}
		next, err := s.ListOrgs(ctx, orgs.ListFilter{Statuses: pub, Q: tag, Sort: orgs.SortName},
			&orgs.ListAfter{Name: rows[0].Name, ID: rows[0].ID}, 10)
		if err != nil || !slices.Equal(names(next), []string{"Beta", "Gama"}) {
			t.Fatalf("after Alfa = %v, %v", names(next), err)
		}
		rows, err = s.ListOrgs(ctx, orgs.ListFilter{Kinds: []orgs.Kind{orgs.KindCompany}, Q: tag}, nil, 2)
		if err != nil || !slices.Equal(names(rows), []string{"Delt", "Gama"}) {
			t.Fatalf("companies newest first = %v, %v", names(rows), err)
		}
		rows, err = s.ListOrgs(ctx, orgs.ListFilter{Kinds: []orgs.Kind{orgs.KindCompany}, Q: tag}, &orgs.ListAfter{ID: rows[1].ID}, 2)
		if err != nil || !slices.Equal(names(rows), []string{"Beta"}) {
			t.Fatalf("companies, second page = %v, %v", names(rows), err)
		}
		// Part of a name, case and Turkish letters folded.
		rows, err = s.ListOrgs(ctx, orgs.ListFilter{Q: "ŞİRKETİ " + tag}, nil, 10)
		if err != nil || len(rows) != 3 {
			t.Fatalf("q folded = %v, %v", names(rows), err)
		}
		rows, err = s.ListOrgs(ctx, orgs.ListFilter{OrgIDs: []uuid.UUID{ids[0], ids[3]}}, nil, 10)
		if err != nil || len(rows) != 2 {
			t.Fatalf("OrgIDs = %v, %v", names(rows), err)
		}
		if rows, err := s.ListOrgs(ctx, orgs.ListFilter{OrgIDs: []uuid.UUID{}}, nil, 10); err != nil || len(rows) != 0 {
			t.Fatalf("empty OrgIDs matched %d rows, %v", len(rows), err)
		}
	})

	t.Run("list: name order is Turkish collation (T-203)", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		ctx := t.Context()
		tag := "q" + strconv.FormatInt(slugSeq.Add(1), 10)
		for _, n := range []string{"Zeytin", "Öykü", "İpek", "Çınar", "Oya", "Işık", "Cem", "ılgın", "ilke"} {
			insert(t, s, newOrg(orgs.KindClub, n+" "+tag))
		}
		f := orgs.ListFilter{Q: tag, Sort: orgs.SortName}
		var got []string
		var after *orgs.ListAfter
		for range 10 {
			rows, err := s.ListOrgs(ctx, f, after, 4)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range rows {
				got = append(got, strings.TrimSuffix(o.Name, " "+tag))
			}
			if len(rows) < 4 {
				break
			}
			last := rows[len(rows)-1]
			after = &orgs.ListAfter{Name: last.Name, ID: last.ID}
		}
		want := []string{"Cem", "Çınar", "ılgın", "Işık", "ilke", "İpek", "Oya", "Öykü", "Zeytin"} // ı = I and i = İ at the first level
		if !slices.Equal(got, want) {
			t.Fatalf("Turkish order = %v, want %v", got, want)
		}
	})
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(*testing.T) (orgs.Store, world) {
		var tick atomic.Int64
		clk := func() time.Time { return t0.Add(time.Duration(tick.Add(1)) * time.Second) }
		return orgs.NewMemStore(clk), memWorld()
	})
}
