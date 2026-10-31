package orgs_test

import (
	"cmp"
	"slices"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orgs"
)

func TestStatsStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	statsContract(t, func(*testing.T) (orgs.Store, world) {
		var tick atomic.Int64
		clk := func() time.Time { return t0.Add(time.Duration(tick.Add(1)) * time.Second) }
		return orgs.NewMemStore(clk), memWorld()
	})
}

// statsContract is the org counts of the panels (BE-163): orgs by kind and
// status and new in a range (a fresh database may hold seed orgs: the
// contract checks what its own rows add); one org's members now, joins and
// departures by Istanbul month, a role change counting as neither.
func statsContract(t *testing.T, newStore func(t *testing.T) (orgs.Store, world)) {
	t.Helper()
	wideFrom, wideTo := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("org stats", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		ctx := t.Context()
		count := func(st orgs.OrgStats, k orgs.Kind, status orgs.Status) int {
			i := slices.IndexFunc(st.Counts, func(c orgs.OrgCount) bool { return c.Kind == k && c.Status == status })
			if i < 0 {
				return 0
			}
			return st.Counts[i].Count
		}
		before, err := s.OrgStats(ctx, wideFrom, wideTo)
		if err != nil {
			t.Fatal(err)
		}
		pending := newOrg(orgs.KindCompany, "Bekleyen")
		pending.Status = orgs.StatusPendingReview
		for _, o := range []orgs.Org{newOrg(orgs.KindClub, "Kulüp 1"), newOrg(orgs.KindClub, "Kulüp 2"), pending} {
			if _, ok, err := s.InsertOrg(ctx, o); err != nil || !ok {
				t.Fatalf("InsertOrg: %v %v", ok, err)
			}
		}
		after, err := s.OrgStats(ctx, wideFrom, wideTo)
		if err != nil {
			t.Fatal(err)
		}
		if d := count(after, orgs.KindClub, orgs.StatusActive) - count(before, orgs.KindClub, orgs.StatusActive); d != 2 {
			t.Errorf("active clubs +%d, want +2", d)
		}
		if d := count(after, orgs.KindCompany, orgs.StatusPendingReview) - count(before, orgs.KindCompany, orgs.StatusPendingReview); d != 1 {
			t.Errorf("pending companies +%d, want +1", d)
		}
		if d := after.NewInRange[orgs.KindClub] - before.NewInRange[orgs.KindClub]; d != 2 {
			t.Errorf("new clubs +%d, want +2", d)
		}
		if !slices.IsSortedFunc(after.Counts, func(a, b orgs.OrgCount) int {
			return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Status, b.Status))
		}) {
			t.Errorf("counts are not by kind then status: %+v", after.Counts)
		}
		empty, err := s.OrgStats(ctx, wideFrom, wideFrom.Add(time.Hour))
		if err != nil || len(empty.NewInRange) != 0 {
			t.Errorf("an empty range: %+v %v", empty.NewInRange, err)
		}
	})

	t.Run("member stats", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		club := newOrg(orgs.KindClub, "Büyüyen Kulüp")
		if _, ok, err := s.InsertOrg(ctx, club); err != nil || !ok {
			t.Fatalf("InsertOrg: %v %v", ok, err)
		}
		// Istanbul is UTC+3: 2026-05-31 22:30Z is June in Istanbul.
		may, june, july := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC), time.Date(2026, 5, 31, 22, 30, 0, 0, time.UTC),
			time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)
		insert := func(m orgs.Membership) orgs.Membership {
			t.Helper()
			got, err := s.InsertMember(ctx, m)
			if err != nil {
				t.Fatalf("InsertMember: %v", err)
			}
			return got
		}
		active := func(user uuid.UUID, role authz.MemberRole, at time.Time) orgs.Membership {
			return insert(orgs.Membership{ID: uuid.NewV7(), OrgID: club.ID, OrgKind: orgs.KindClub, UserID: user, Role: role,
				Status: orgs.MemberActive, Source: orgs.SourceAdmin, JoinedAt: &at})
		}
		a, b, c, d := w.user(t), w.user(t), w.user(t), w.user(t)
		active(a, authz.MemberMember, may)
		leaver := active(b, authz.MemberMember, june)
		promoted := active(c, authz.MemberMember, may)
		// c becomes club_admin in July: the old row ends role_changed, a new one starts at the same instant.
		if _, err := s.EndMember(ctx, club.ID, promoted.ID, july, orgs.EndRoleChanged); err != nil {
			t.Fatal(err)
		}
		active(c, authz.MemberClubAdmin, july)
		if _, err := s.EndMember(ctx, club.ID, leaver.ID, july, orgs.EndLeft); err != nil {
			t.Fatal(err)
		}
		req := may
		insert(orgs.Membership{ID: uuid.NewV7(), OrgID: club.ID, OrgKind: orgs.KindClub, UserID: d, Role: authz.MemberMember,
			Status: orgs.MemberPending, Source: orgs.SourceJoinRequest, RequestedAt: &req})

		st, err := s.MemberStats(ctx, club.ID, wideFrom, wideTo)
		if err != nil {
			t.Fatal(err)
		}
		if st.Active != 2 || st.Pending != 1 || st.JoinedInRange != 3 || st.LeftInRange != 1 {
			t.Fatalf("stats = %+v, want active 2, pending 1, joined 3, left 1", st)
		}
		want := []orgs.MemberMonth{{Month: "2026-05", Joined: 2}, {Month: "2026-06", Joined: 1}, {Month: "2026-07", Left: 1}}
		if !slices.Equal(st.Monthly, want) {
			t.Fatalf("monthly = %+v, want %+v", st.Monthly, want)
		}
		june1 := time.Date(2026, 5, 31, 21, 0, 0, 0, time.UTC) // 2026-06-01 00:00 Istanbul
		st, err = s.MemberStats(ctx, club.ID, june1, wideTo)
		if err != nil {
			t.Fatal(err)
		}
		if st.JoinedInRange != 1 || st.LeftInRange != 1 || st.Active != 2 {
			t.Fatalf("from June = %+v", st)
		}
		if other, err := s.MemberStats(ctx, uuid.NewV7(), wideFrom, wideTo); err != nil || other.Active != 0 || len(other.Monthly) != 0 {
			t.Fatalf("unknown org = %+v, %v", other, err)
		}
	})
}
