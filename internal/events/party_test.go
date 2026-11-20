package events_test

import (
	"slices"
	"testing"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/orgs"
	"koro/internal/platform/problem/problemtest"
)

// TestEvents_PartyActsAsMember (BE-214, Q-69): a platform admin who is a
// plain member of the organiser club acts on its events as a member: no
// edit, no cancellation, no break-glass read of the registrations; as its
// club_admin they edit, but not while the club is suspended (403
// orgs.org_suspended like any club_admin). A platform admin of no club
// edits the suspended club's event.
func TestEvents_PartyActsAsMember(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.published(t, input("Taraf Etkinliği"))
	join := func(a authz.Actor, role authz.MemberRole) authz.Actor {
		a.Memberships = append(slices.Clone(a.Memberships), authz.Membership{OrgID: orgID("club_a"), OrgKind: authz.OrgClub, Role: role})
		return a
	}
	pad := persona(t, "platform_admin")
	member := join(pad, authz.MemberMember)
	cur, err := e.svc.GetEvent(ctx, pad, v.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	title := "Personel Başlığı"
	patch := events.EventPatch{Title: &title}
	_, err = e.svc.UpdateEvent(ctx, member, v.Event.ID, cur.ETag, patch)
	wantForbidden(t, err)
	_, err = e.svc.CancelEvent(ctx, member, v.Event.ID, "Kendi kulübüm")
	wantForbidden(t, err)
	_, err = e.svc.ListEventRegistrations(ctx, member, v.Event.ID, events.EventRegistrationQuery{},
		events.Access{Reason: "Destek talebinin incelenmesi"})
	wantForbidden(t, err)

	e.orgs.setStatus(orgID("club_a"), orgs.StatusSuspended)
	_, err = e.svc.UpdateEvent(ctx, join(pad, authz.MemberClubAdmin), v.Event.ID, cur.ETag, patch)
	problemtest.WantProblem(t, err, 403, "orgs.org_suspended")
	got, err := e.svc.UpdateEvent(ctx, pad, v.Event.ID, cur.ETag, patch)
	if err != nil || got.Event.Title != title {
		t.Fatalf("edit by a platform admin of no club = %+v, %v", got.Event, err)
	}
}
