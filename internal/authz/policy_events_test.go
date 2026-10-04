package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// eventRes is the event resource the events service builds (03 §3.5; 06
// H-27): the organiser, the status and visibility, the sensitive flag and
// the caller's staff role (persona event_staff is event_a's scanner).
func eventRes(org, status, visibility, uni string, staffRole func(authztest.Persona) string) func(authztest.Persona) authz.Resource {
	return func(p authztest.Persona) authz.Resource {
		id := authztest.ID(org)
		u := ""
		if uni != "" {
			u = authztest.UniversityID(uni).String()
		}
		role := "none"
		if staffRole != nil {
			role = staffRole(p)
		}
		return authz.Resource{Type: authz.TypeEvent, ID: authztest.ID("event_" + org), OrgID: &id, Attrs: map[string]string{
			authz.AttrStatus: status, "visibility": visibility, "visibility_university_id": u,
			authz.AttrSensitive: "false", authz.AttrEventStaffRole: role,
		}}
	}
}

// staffOfEventA: persona event_staff scans event_a (03 §7.3).
func staffOfEventA(p authztest.Persona) string {
	if p.Name == "event_staff" {
		return "scanner"
	}
	return "none"
}

// formRes is the event_form resource the events service builds for the
// form actions (BE-026): the event's resource as a form sub-surface, with
// event_status.
func formRes(org, status, visibility, uni string, staffRole func(authztest.Persona) string) func(authztest.Persona) authz.Resource {
	ev := eventRes(org, status, visibility, uni, staffRole)
	return func(p authztest.Persona) authz.Resource {
		r := ev(p)
		r.Type = authz.TypeEventForm
		r.Attrs["event_status"] = status
		return r
	}
}

func adminSurfaceRes(authztest.Persona) authz.Resource {
	return authz.Resource{Type: authz.TypeAdminSurface}
}

func orgSurface(name string, kind authz.OrgKind) func(authztest.Persona) authz.Resource {
	return func(authztest.Persona) authz.Resource { return orgRes(name, kind, "active") }
}

// TestEventsPolicy_Matrix pins 03 §5.6 (BE-024's 10 and BE-026's 4 actions) for the 18
// personas; testdata/authz/events.yaml checks the same through the API.
func TestEventsPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fb, nf := authz.ErrForbidden, authz.ErrNotFound
	all := func(err error) map[string]error {
		m := map[string]error{}
		for _, n := range authztest.PersonaNames {
			m[n] = err
		}
		return m
	}
	// hidden: everyone 404 except anonymous, whom the core refuses first on a
	// non-public action (403; 401 over HTTP).
	hidden := func() map[string]error {
		m := all(nf)
		m["anonymous"] = fb
		return m
	}
	with := func(base map[string]error, over map[string]error) map[string]error {
		for k, v := range over {
			base[k] = v
		}
		return base
	}
	staff := []string{"moderator", "support", "dpo", "platform_admin", "finance_admin", "talent_auditor"}
	platformSees := func(m map[string]error, allowed ...string) map[string]error {
		for _, s := range staff {
			m[s] = fb
		}
		for _, a := range allowed {
			m[a] = nil
		}
		return m
	}
	eventA := eventRes("club_a", "published", "public", "", staffOfEventA)
	members := eventRes("club_a", "published", "members", "", staffOfEventA)
	draft := eventRes("club_a", "draft", "public", "", nil)
	uniEvent := eventRes("uni_a", "published", "university", "uni_a", nil)
	cancelled := eventRes("club_a", "cancelled", "public", "", nil)
	// The members of club_a and its event staff see a members event; the
	// students of uni_a also see a university event of uni_a.
	membersVisible := map[string]error{"club_member": fb, "club_admin": fb, "event_staff": fb}
	uniVisible := map[string]error{"student": fb, "student_minor": fb, "club_member": fb, "club_admin": fb,
		"event_staff": fb, "university_rep": fb}
	for _, tc := range []struct {
		name string
		act  authz.Action
		res  func(authztest.Persona) authz.Resource
		want map[string]error
	}{
		{"list", authz.EventsEventList, func(authztest.Persona) authz.Resource {
			return authz.Resource{Type: authz.TypeEventCatalog}
		}, all(nil)},
		{"read public", authz.EventsEventRead, eventA, all(nil)},
		{"read cancelled", authz.EventsEventRead, cancelled, all(nil)},
		{"read members", authz.EventsEventRead, members,
			with(all(nf), map[string]error{"club_member": nil, "club_admin": nil, "event_staff": nil, "moderator": nil,
				"support": nil, "dpo": nil, "platform_admin": nil, "finance_admin": nil, "talent_auditor": nil})},
		{"read draft", authz.EventsEventRead, draft,
			platformSees(with(all(nf), map[string]error{"club_admin": nil}), staff...)},
		{"read university", authz.EventsEventRead, uniEvent,
			platformSees(with(all(nf), map[string]error{"student": nil, "student_minor": nil, "club_member": nil,
				"club_admin": nil, "event_staff": nil, "university_rep": nil}), staff...)},
		{"list_org club_a", authz.EventsEventListOrg, orgSurface("club_a", authz.OrgClub),
			platformSees(with(hidden(), map[string]error{"club_member": fb, "club_admin": nil}), "moderator", "support", "platform_admin")},
		{"create club_a", authz.EventsEventCreate, orgSurface("club_a", authz.OrgClub),
			platformSees(with(hidden(), map[string]error{"club_member": fb, "club_admin": nil}))},
		{"create uni_a", authz.EventsEventCreate, orgSurface("uni_a", authz.OrgUniversity),
			platformSees(with(hidden(), map[string]error{"university_rep": nil}))},
		{"create company_a (k13)", authz.EventsEventCreate, orgSurface("company_a", authz.OrgCompany),
			platformSees(with(hidden(), map[string]error{"company_member": fb, "company_admin": fb}))},
		{"update public", authz.EventsEventUpdate, eventA, with(all(fb), map[string]error{"club_admin": nil, "platform_admin": nil})},
		{"update members", authz.EventsEventUpdate, members,
			platformSees(with(hidden(), with(membersVisible, map[string]error{"club_admin": nil})), "platform_admin")},
		{"update draft", authz.EventsEventUpdate, draft, platformSees(with(hidden(), map[string]error{"club_admin": nil}), "platform_admin")},
		{"update university", authz.EventsEventUpdate, uniEvent,
			platformSees(with(hidden(), with(uniVisible, map[string]error{"university_rep": nil})), "platform_admin")},
		{"publish draft", authz.EventsEventPublish, draft, platformSees(with(hidden(), map[string]error{"club_admin": nil}))},
		{"cancel public", authz.EventsEventCancel, eventA, with(all(fb), map[string]error{"club_admin": nil, "platform_admin": nil})},
		{"delete draft", authz.EventsEventDelete, draft, platformSees(with(hidden(), map[string]error{"club_admin": nil}))},
		{"staff.list public", authz.EventsStaffList, eventA, with(all(fb), map[string]error{"club_admin": nil})},
		{"staff.manage public", authz.EventsStaffManage, eventA, with(all(fb), map[string]error{"club_admin": nil})},
		{"staff.manage university", authz.EventsStaffManage, uniEvent,
			platformSees(with(hidden(), with(uniVisible, map[string]error{"university_rep": nil})))},
		// BE-026: the published form follows the event's visibility; the
		// draft schema and its publication are the organiser's management
		// sub-surface (another org's form: 404).
		{"form.read public", authz.EventsFormRead, formRes("club_a", "published", "public", "", staffOfEventA), all(nil)},
		{"form.read members", authz.EventsFormRead, formRes("club_a", "published", "members", "", staffOfEventA),
			with(all(nf), map[string]error{"club_member": nil, "club_admin": nil, "event_staff": nil, "moderator": nil,
				"support": nil, "dpo": nil, "platform_admin": nil, "finance_admin": nil, "talent_auditor": nil})},
		{"form.read draft", authz.EventsFormRead, formRes("club_a", "draft", "public", "", nil),
			with(all(nf), map[string]error{"club_admin": nil, "moderator": nil, "support": nil, "dpo": nil,
				"platform_admin": nil, "finance_admin": nil, "talent_auditor": nil})},
		{"form.manage public", authz.EventsFormManage, formRes("club_a", "published", "public", "", staffOfEventA),
			platformSees(with(hidden(), map[string]error{"club_member": fb, "club_admin": nil}))},
		{"form.manage draft", authz.EventsFormManage, formRes("club_a", "draft", "public", "", nil),
			platformSees(with(hidden(), map[string]error{"club_admin": nil}))},
		{"form.manage university", authz.EventsFormManage, formRes("uni_a", "published", "university", "uni_a", nil),
			platformSees(with(hidden(), map[string]error{"university_rep": nil}))},
		{"form.publish public", authz.EventsFormPublish, formRes("club_a", "published", "public", "", staffOfEventA),
			platformSees(with(hidden(), map[string]error{"club_member": fb, "club_admin": nil}))},
		{"form.publish members", authz.EventsFormPublish, formRes("club_a", "published", "members", "", staffOfEventA),
			platformSees(with(hidden(), map[string]error{"club_member": fb, "club_admin": nil}))},
		{"lint_rule.manage", authz.EventsLintRuleManage, adminSurfaceRes,
			with(all(fb), map[string]error{"dpo": nil, "platform_admin": nil})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, p := range authztest.Personas() {
				want, ok := tc.want[p.Name]
				if !ok {
					t.Fatalf("no expectation for %s", p.Name)
				}
				t.Run(p.Name, func(t *testing.T) {
					wantErr(t, authz.Can(ctx, p.Actor, tc.act, tc.res(p)), want, false)
				})
			}
		})
	}
}

func TestEventsPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	draft := eventRes("club_a", "draft", "public", "", nil)
	uniEvent := eventRes("uni_a", "published", "public", "", nil)
	p := authztest.Personas()[0]
	org0 := authztest.ID("club_a")
	// k-po: a platform_admin who belongs to the platform org creates,
	// publishes and manages its events.
	pad := persona(t, "platform_admin")
	pad.Memberships = append(pad.Memberships, authz.Membership{OrgID: authztest.ID("platform_org"), OrgKind: authz.OrgPlatform,
		Role: authz.MemberStaff})
	platformEvent := eventRes("platform_org", "draft", "public", "", nil)
	for _, act := range []authz.Action{authz.EventsEventPublish, authz.EventsEventDelete, authz.EventsStaffManage, authz.EventsStaffList} {
		wantErr(t, authz.Can(ctx, pad, act, platformEvent(p)), nil, false)
	}
	wantErr(t, authz.Can(ctx, pad, authz.EventsEventCreate, orgRes("platform_org", authz.OrgPlatform, "active")), nil, false)
	// career_staff holds the university cells of events, without MFA.
	cs := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(),
		Memberships: []authz.Membership{{OrgID: authztest.ID("uni_a"), OrgKind: authz.OrgUniversity, Role: authz.MemberCareerStaff}}}
	for _, act := range []authz.Action{authz.EventsEventUpdate, authz.EventsEventCancel, authz.EventsStaffManage} {
		wantErr(t, authz.Can(ctx, cs, act, uniEvent(p)), nil, false)
	}
	// university_rep needs its MFA session (k-mfa).
	rep := persona(t, "university_rep")
	rep.MFAVerified = false
	wantErr(t, authz.Can(ctx, rep, authz.EventsEventUpdate, uniEvent(p)), authz.ErrMFARequired, false)
	// A draft of another club is invisible to its admin (06 H-38: 404).
	wantErr(t, authz.Can(ctx, persona(t, "other_club_admin"), authz.EventsEventUpdate, draft(p)), authz.ErrNotFound, false)
	// The event staff role gives no management action (03 §5.6: E cells are BE-031's).
	staffActor := persona(t, "event_staff")
	members := eventRes("club_a", "published", "members", "", staffOfEventA)
	sp, _ := authztest.Get("event_staff")
	wantErr(t, authz.Can(ctx, staffActor, authz.EventsStaffList, members(sp)), authz.ErrForbidden, false)
	// An unknown status or visibility hides the event (safe default).
	wantErr(t, authz.Can(ctx, authz.Anonymous(), authz.EventsEventRead, eventRes("club_a", "archived", "public", "", nil)(p)),
		authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, authz.Anonymous(), authz.EventsEventRead, eventRes("club_a", "published", "friends", "", nil)(p)),
		authz.ErrNotFound, false)
	// BE-026: k-po for the platform org's forms, career_staff on its
	// university's forms, MFA of the lint rule managers, and the form
	// resource needs event_status.
	platformForm := formRes("platform_org", "draft", "public", "", nil)
	for _, act := range []authz.Action{authz.EventsFormManage, authz.EventsFormPublish} {
		wantErr(t, authz.Can(ctx, pad, act, platformForm(p)), nil, false)
		wantErr(t, authz.Can(ctx, cs, act, formRes("uni_a", "published", "public", "", nil)(p)), nil, false)
		// A form of another org is invisible to a club admin (404), even
		// when the event itself is public.
		wantErr(t, authz.Can(ctx, persona(t, "other_club_admin"), act, formRes("club_a", "published", "public", "", nil)(p)),
			authz.ErrNotFound, false)
	}
	dpo := persona(t, "dpo")
	dpo.MFAVerified = false
	wantErr(t, authz.Can(ctx, dpo, authz.EventsLintRuleManage, adminSurfaceRes(p)), authz.ErrMFARequired, false)
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.EventsFormManage,
		authz.Resource{Type: authz.TypeEventForm, OrgID: &org0, Attrs: map[string]string{authz.AttrStatus: "draft"}}),
		authz.ErrForbidden, true)
	// Missing Attrs are a programmer error (deny).
	org := authztest.ID("club_a")
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.EventsEventUpdate,
		authz.Resource{Type: authz.TypeEvent, OrgID: &org, Attrs: map[string]string{authz.AttrStatus: "draft"}}), authz.ErrForbidden, true)
}
