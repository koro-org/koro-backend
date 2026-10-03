package orgs_test

import (
	"errors"
	"slices"
	"strconv"
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
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(*testing.T) (orgs.Store, world) {
		var tick atomic.Int64
		clk := func() time.Time { return t0.Add(time.Duration(tick.Add(1)) * time.Second) }
		return orgs.NewMemStore(clk), memWorld()
	})
}
