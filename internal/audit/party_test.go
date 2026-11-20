package audit_test

import (
	"slices"
	"testing"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/platform/httpx"
)

// TestListOrgAuditEvents_PartyReadsAsMember (BE-214, Q-69): a dpo or
// platform admin who is the club's club_admin reads its log as its
// club_admin, without IP and user agent; as a plain member they are refused.
// A dpo of no club keeps the network facts.
func TestListOrgAuditEvents_PartyReadsAsMember(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	org := authztest.ID("club_a")
	ctx := httpx.WithUserAgent(httpx.WithClientIP(t.Context(), "203.0.113.9"), "Mozilla/5.0 Firefox/141.0")
	e.logN(t, ctx, 2, "events.event.create", authztest.ID("club_admin"), &org)
	from, to := window()
	q := audit.OrgQuery{PageQuery: audit.PageQuery{Limit: 10}, From: from, To: to}
	join := func(a authz.Actor, role authz.MemberRole) authz.Actor {
		a.Memberships = append(slices.Clone(a.Memberships), authz.Membership{OrgID: org, OrgKind: authz.OrgClub, Role: role})
		return a
	}
	for _, name := range []string{"dpo", "platform_admin"} {
		page, err := e.svc.ListOrg(ctx, join(persona(t, name), authz.MemberClubAdmin), org, q)
		if err != nil || len(page.Items) != 2 {
			t.Fatalf("%s as club_admin: %d items, %v", name, len(page.Items), err)
		}
		for _, it := range page.Items {
			if it.IP != "" || it.UserAgent != "" {
				t.Fatalf("%s as club_admin reads the network facts: %+v", name, it)
			}
		}
		if _, err := e.svc.ListOrg(ctx, join(persona(t, name), authz.MemberMember), org, q); err == nil {
			t.Fatalf("%s as a plain member read the club's log", name)
		}
	}
	page, err := e.svc.ListOrg(ctx, persona(t, "dpo"), org, q)
	if err != nil || len(page.Items) != 2 || page.Items[0].IP != "203.0.113.9" {
		t.Fatalf("dpo of no club: %+v, %v", page.Items, err)
	}
}
