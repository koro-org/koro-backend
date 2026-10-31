package orgs

import (
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"

	"koro/internal/platform/clock"
)

// OrgStats implements statsStore.
func (m *MemStore) OrgStats(_ context.Context, from, to time.Time) (OrgStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	type key struct {
		kind   Kind
		status Status
	}
	counts := map[key]int{}
	st := OrgStats{Counts: []OrgCount{}, NewInRange: map[Kind]int{}}
	for _, o := range m.orgs {
		counts[key{o.Kind, o.Status}]++
		if inRange(o.CreatedAt, from, to) {
			st.NewInRange[o.Kind]++
		}
	}
	for k, n := range counts {
		st.Counts = append(st.Counts, OrgCount{Kind: k.kind, Status: k.status, Count: n})
	}
	slices.SortFunc(st.Counts, func(a, b OrgCount) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Status, b.Status))
	})
	return st, nil
}

// MemberStats implements statsStore.
func (m *MemStore) MemberStats(_ context.Context, orgID uuid.UUID, from, to time.Time) (MemberStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	loc := clock.Istanbul()
	st := MemberStats{}
	months := map[string]*MemberMonth{}
	successor := func(r Membership) bool { // the new row of a role change
		for _, p := range m.members {
			if p.OrgID == r.OrgID && p.UserID == r.UserID && p.EndReason != nil && *p.EndReason == EndRoleChanged &&
				p.EndedAt != nil && p.EndedAt.Equal(*r.JoinedAt) {
				return true
			}
		}
		return false
	}
	for _, r := range m.members {
		if r.OrgID != orgID {
			continue
		}
		switch r.Status {
		case MemberActive:
			st.Active++
		case MemberPending:
			st.Pending++
		}
		if r.JoinedAt == nil {
			continue
		}
		if inRange(*r.JoinedAt, from, to) && !successor(r) {
			addMonth(months, monthKey(*r.JoinedAt, loc)).Joined++
			st.JoinedInRange++
		}
		if r.EndedAt != nil && inRange(*r.EndedAt, from, to) && (r.EndReason == nil || *r.EndReason != EndRoleChanged) {
			addMonth(months, monthKey(*r.EndedAt, loc)).Left++
			st.LeftInRange++
		}
	}
	st.Monthly = sortedMonths(months)
	return st, nil
}

// inRange reports whether t is in [from, to).
func inRange(t, from, to time.Time) bool { return !t.Before(from) && t.Before(to) }
