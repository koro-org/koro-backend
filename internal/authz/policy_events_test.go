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

// withReason adds a break-glass reason to a resource builder.
func withReason(res func(authztest.Persona) authz.Resource, reason string) func(authztest.Persona) authz.Resource {
	return func(p authztest.Persona) authz.Resource {
		r := res(p)
		r.Attrs[authz.AttrAccessReason] = reason
		return r
	}
}

// ownRegistration is the registration resource of persona owner's own
// registration of event_a (BE-028).
func ownRegistration(owner string) func(authztest.Persona) authz.Resource {
	return func(authztest.Persona) authz.Resource {
		org, u := authztest.ID("club_a"), authztest.ID(owner)
		return authz.Resource{Type: authz.TypeRegistration, ID: authztest.ID("reg_" + owner), OrgID: &org, OwnerUserID: &u,
			Attrs: map[string]string{authz.AttrStatus: "confirmed", authz.AttrEventStaffRole: "none"}}
	}
}

// TestEventsPolicy_Registrations pins 03 §5.6 for BE-028's 8 actions: only
// verified students register (a university event: its students); the own
// actions are O; the organiser's list/read/decide/cancel are S on the
// event's sub-surface, list/read also by break-glass (support,
// platform_admin) except on a sensitive event (Y2 → 404).
func TestEventsPolicy_Registrations(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fb, nf, bg := authz.ErrForbidden, authz.ErrNotFound, authz.ErrBreakGlassReason
	all := func(err error) map[string]error {
		m := map[string]error{}
		for _, n := range authztest.PersonaNames {
			m[n] = err
		}
		return m
	}
	with := func(base map[string]error, over map[string]error) map[string]error {
		for k, v := range over {
			base[k] = v
		}
		return base
	}
	eventA := eventRes("club_a", "published", "public", "", staffOfEventA)
	members := eventRes("club_a", "published", "members", "", staffOfEventA)
	uniEvent := eventRes("uni_a", "published", "university", "uni_a", nil)
	sensitive := func(p authztest.Persona) authz.Resource {
		r := eventRes("club_sensitive", "published", "public", "", nil)(p)
		r.Attrs[authz.AttrSensitive] = "true"
		return r
	}
	students := map[string]error{"student": nil, "student_minor": nil, "club_member": nil, "club_admin": nil,
		"other_club_admin": nil, "event_staff": nil}
	hiddenMembers := map[string]error{"user_unverified": nf, "student": nf, "student_minor": nf, "other_club_admin": nf,
		"university_rep": nf, "company_member": nf, "company_admin": nf, "other_company_member": nf}
	hiddenUni := map[string]error{"user_unverified": nf, "other_club_admin": nf, "company_member": nf, "company_admin": nf,
		"other_company_member": nf}
	why := "Destek talebi 4711: kayıt sorunu inceleniyor"
	for _, tc := range []struct {
		name string
		act  authz.Action
		res  func(authztest.Persona) authz.Resource
		want map[string]error
	}{
		{"create public", authz.EventsRegistrationCreate, eventA, with(all(fb), students)},
		{"create members", authz.EventsRegistrationCreate, members,
			with(with(all(fb), hiddenMembers), map[string]error{"club_member": nil, "club_admin": nil, "event_staff": nil})},
		{"create university", authz.EventsRegistrationCreate, uniEvent,
			with(with(all(fb), hiddenUni), map[string]error{"student": nil, "student_minor": nil, "club_member": nil,
				"club_admin": nil, "event_staff": nil})},
		{"list_own", authz.EventsRegistrationListOwn, ownRegistration("student"),
			with(all(nf), map[string]error{"anonymous": fb, "student": nil})},
		{"read_own", authz.EventsRegistrationReadOwn, ownRegistration("club_member"),
			with(all(nf), map[string]error{"anonymous": fb, "club_member": nil})},
		{"cancel_own", authz.EventsRegistrationCancelOwn, ownRegistration("company_admin"),
			with(all(nf), map[string]error{"anonymous": fb, "company_admin": nil})},
		{"list public", authz.EventsRegistrationList, eventA,
			with(all(fb), map[string]error{"club_admin": nil, "support": bg, "platform_admin": bg})},
		{"list public with a reason", authz.EventsRegistrationList, withReason(eventA, why),
			with(all(fb), map[string]error{"club_admin": nil, "support": nil, "platform_admin": nil})},
		{"read public with a short reason", authz.EventsRegistrationRead, withReason(eventA, "kısa"),
			with(all(fb), map[string]error{"club_admin": nil, "support": bg, "platform_admin": bg})},
		{"read members", authz.EventsRegistrationRead, withReason(members, why),
			with(with(all(fb), hiddenMembers), map[string]error{"anonymous": fb, "club_admin": nil, "support": nil,
				"platform_admin": nil})},
		{"list university", authz.EventsRegistrationList, uniEvent,
			with(with(all(fb), hiddenUni), map[string]error{"university_rep": nil, "support": bg, "platform_admin": bg})},
		{"list sensitive by break-glass", authz.EventsRegistrationList, withReason(sensitive, why),
			with(all(fb), map[string]error{"support": nf, "platform_admin": nf})},
		{"decide public", authz.EventsRegistrationDecide, eventA, with(all(fb), map[string]error{"club_admin": nil})},
		{"cancel members", authz.EventsRegistrationCancel, members,
			with(with(all(fb), hiddenMembers), map[string]error{"club_admin": nil})},
		{"cancel university", authz.EventsRegistrationCancel, uniEvent,
			with(with(all(fb), hiddenUni), map[string]error{"university_rep": nil})},
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
	p := authztest.Personas()[0]
	// k-po: a platform_admin of the platform org manages its registrations
	// without a reason.
	pad := persona(t, "platform_admin")
	pad.Memberships = append(pad.Memberships, authz.Membership{OrgID: authztest.ID("platform_org"), OrgKind: authz.OrgPlatform,
		Role: authz.MemberStaff})
	platformEvent := eventRes("platform_org", "published", "public", "", nil)
	for _, act := range []authz.Action{authz.EventsRegistrationList, authz.EventsRegistrationRead,
		authz.EventsRegistrationDecide, authz.EventsRegistrationCancel} {
		wantErr(t, authz.Can(ctx, pad, act, platformEvent(p)), nil, false)
	}
	// career_staff decides its university's applications; a minor club
	// admin is no club_admin (Y4) but registers as a student.
	cs := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(),
		Memberships: []authz.Membership{{OrgID: authztest.ID("uni_a"), OrgKind: authz.OrgUniversity, Role: authz.MemberCareerStaff}}}
	wantErr(t, authz.Can(ctx, cs, authz.EventsRegistrationDecide, uniEvent(p)), nil, false)
	minorAdmin := persona(t, "club_admin")
	minorAdmin.IsMinor = true
	wantErr(t, authz.Can(ctx, minorAdmin, authz.EventsRegistrationDecide, eventA(p)), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, minorAdmin, authz.EventsRegistrationCreate, eventA(p)), nil, false)
	// A student of another university who is a member of the organiser
	// sees a university event but cannot register for it.
	stranger := persona(t, "other_club_admin")
	stranger.Memberships = append(stranger.Memberships, authz.Membership{OrgID: authztest.ID("uni_a"),
		OrgKind: authz.OrgUniversity, Role: authz.MemberStaff})
	wantErr(t, authz.Can(ctx, stranger, authz.EventsRegistrationCreate, uniEvent(p)), authz.ErrForbidden, false)
	// support without MFA is refused before the reason (03 §3.2-5).
	sup := persona(t, "support")
	sup.MFAVerified = false
	wantErr(t, authz.Can(ctx, sup, authz.EventsRegistrationList, withReason(eventA, why)(p)), authz.ErrMFARequired, false)
	// A draft event's registrations are its managers' only.
	draft := eventRes("club_a", "draft", "public", "", nil)
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.EventsRegistrationCreate, draft(p)), authz.ErrNotFound, false)
}

// ownTicket is the ticket resource of persona owner's own registration of
// event_a (BE-031).
func ownTicket(owner string) func(authztest.Persona) authz.Resource {
	return func(authztest.Persona) authz.Resource {
		org, u := authztest.ID("club_a"), authztest.ID(owner)
		return authz.Resource{Type: authz.TypeTicket, ID: authztest.ID("reg_" + owner), OrgID: &org, OwnerUserID: &u,
			Attrs: map[string]string{}}
	}
}

// TestEventsPolicy_TicketsAndCheckins pins 03 §5.6 for BE-031's 8 actions:
// the ticket is O (reissue also S for the organiser's managers on the
// event's sub-surface); the door (create, list, stats) is S for the
// organiser's managers and E for the event's staff, void and finalize Em
// (manager only); platform_admin only for the platform org (k-po).
func TestEventsPolicy_TicketsAndCheckins(t *testing.T) {
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
	with := func(base map[string]error, over map[string]error) map[string]error {
		for k, v := range over {
			base[k] = v
		}
		return base
	}
	eventA := eventRes("club_a", "published", "public", "", staffOfEventA)
	members := eventRes("club_a", "published", "members", "", staffOfEventA)
	uniEvent := eventRes("uni_a", "published", "university", "uni_a", nil)
	hiddenMembers := map[string]error{"user_unverified": nf, "student": nf, "student_minor": nf, "other_club_admin": nf,
		"university_rep": nf, "company_member": nf, "company_admin": nf, "other_company_member": nf}
	hiddenUni := map[string]error{"user_unverified": nf, "other_club_admin": nf, "company_member": nf, "company_admin": nf,
		"other_company_member": nf}
	for _, tc := range []struct {
		name string
		act  authz.Action
		res  func(authztest.Persona) authz.Resource
		want map[string]error
	}{
		{"read_own", authz.EventsTicketReadOwn, ownTicket("student"), with(all(nf), map[string]error{"anonymous": fb, "student": nil})},
		{"resend", authz.EventsTicketResend, ownTicket("platform_admin"),
			with(all(nf), map[string]error{"anonymous": fb, "platform_admin": nil})},
		{"reissue own", authz.EventsTicketReissue, ownTicket("event_staff"),
			with(all(nf), map[string]error{"anonymous": fb, "event_staff": nil})},
		{"reissue by the organiser", authz.EventsTicketReissue, eventA, with(all(fb), map[string]error{"club_admin": nil})},
		{"reissue university", authz.EventsTicketReissue, uniEvent,
			with(with(all(fb), hiddenUni), map[string]error{"university_rep": nil})},
		{"checkin create", authz.EventsCheckinCreate, eventA,
			with(all(fb), map[string]error{"club_admin": nil, "event_staff": nil})},
		{"checkin list members", authz.EventsCheckinList, members,
			with(with(all(fb), hiddenMembers), map[string]error{"club_admin": nil, "event_staff": nil})},
		{"checkin stats university", authz.EventsCheckinStats, uniEvent,
			with(with(all(fb), hiddenUni), map[string]error{"university_rep": nil})},
		{"checkin void: scanner is not enough", authz.EventsCheckinVoid, eventA,
			with(all(fb), map[string]error{"club_admin": nil})},
		{"finalize", authz.EventsAttendanceFinalize, eventA, with(all(fb), map[string]error{"club_admin": nil})},
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
	p := authztest.Personas()[0]
	// Em: the event's manager voids and finalizes; a scanner does not.
	mgr := persona(t, "event_manager_a")
	managed := func(role string) authz.Resource {
		r := eventRes("club_a", "published", "public", "", nil)(p)
		r.Attrs[authz.AttrEventStaffRole] = role
		return r
	}
	for _, act := range []authz.Action{authz.EventsCheckinVoid, authz.EventsAttendanceFinalize, authz.EventsCheckinCreate,
		authz.EventsCheckinList, authz.EventsCheckinStats} {
		wantErr(t, authz.Can(ctx, mgr, act, managed("manager")), nil, false)
	}
	scanner := persona(t, "event_staff")
	wantErr(t, authz.Can(ctx, scanner, authz.EventsAttendanceFinalize, managed("scanner")), fb, false)
	// Staff of another event (Attrs none) get nothing; the staff grant is
	// not the reissue grant.
	wantErr(t, authz.Can(ctx, scanner, authz.EventsCheckinCreate, managed("none")), fb, false)
	wantErr(t, authz.Can(ctx, mgr, authz.EventsTicketReissue, managed("manager")), fb, false)
	// k-po: a platform_admin of the platform org runs the door of its events.
	pad := persona(t, "platform_admin")
	pad.Memberships = append(pad.Memberships, authz.Membership{OrgID: authztest.ID("platform_org"), OrgKind: authz.OrgPlatform,
		Role: authz.MemberStaff})
	platformEvent := eventRes("platform_org", "published", "public", "", nil)
	for _, act := range []authz.Action{authz.EventsCheckinCreate, authz.EventsCheckinVoid, authz.EventsCheckinList,
		authz.EventsCheckinStats, authz.EventsAttendanceFinalize} {
		wantErr(t, authz.Can(ctx, pad, act, platformEvent(p)), nil, false)
	}
	// ... but reissues only its own tickets (03 §5.6: pad O).
	wantErr(t, authz.Can(ctx, pad, authz.EventsTicketReissue, platformEvent(p)), fb, false)
	// The system closes attendances (03 §3.7); it scans nothing.
	sys := authz.SystemActor("events.auto_finalize_attendance")
	wantErr(t, authz.Can(ctx, sys, authz.EventsAttendanceFinalize, authz.Resource{}), nil, false)
	wantErr(t, authz.Can(ctx, sys, authz.EventsCheckinCreate, authz.Resource{}), fb, false)
	// A draft event's door is its managers' only.
	draft := eventRes("club_a", "draft", "public", "", staffOfEventA)
	wantErr(t, authz.Can(ctx, scanner, authz.EventsCheckinCreate, draft(p)), nf, false)
}

// exportRes is the event_export resource the events service builds for
// downloadEventExport (BE-050): the event's resource with the export's ID
// and its requester as owner.
func exportRes(org, visibility, owner string, staffRole func(authztest.Persona) string) func(authztest.Persona) authz.Resource {
	ev := eventRes(org, "published", visibility, "", staffRole)
	return func(p authztest.Persona) authz.Resource {
		r := ev(p)
		by := authztest.ID(owner)
		r.Type, r.ID, r.OwnerUserID = authz.TypeEventExport, authztest.ID("export_"+org), &by
		return r
	}
}

// TestEventsPolicy_Exports pins 03 §5.6 for BE-050's 3 actions: create
// (S·R; R is the service's) and list (S, dpo ✓) on the event's
// sub-surface; download S on the event, then S·O on the export (only its
// requester among the managers); platform_admin only for the platform org
// (k-po).
func TestEventsPolicy_Exports(t *testing.T) {
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
	with := func(base map[string]error, over map[string]error) map[string]error {
		for k, v := range over {
			base[k] = v
		}
		return base
	}
	eventA := eventRes("club_a", "published", "public", "", staffOfEventA)
	members := eventRes("club_a", "published", "members", "", staffOfEventA)
	uniEvent := eventRes("uni_a", "published", "university", "uni_a", nil)
	hiddenMembers := map[string]error{"user_unverified": nf, "student": nf, "student_minor": nf, "other_club_admin": nf,
		"university_rep": nf, "company_member": nf, "company_admin": nf, "other_company_member": nf}
	hiddenUni := map[string]error{"user_unverified": nf, "other_club_admin": nf, "company_member": nf, "company_admin": nf,
		"other_company_member": nf}
	for _, tc := range []struct {
		name string
		act  authz.Action
		res  func(authztest.Persona) authz.Resource
		want map[string]error
	}{
		{"create public", authz.EventsExportCreate, eventA, with(all(fb), map[string]error{"club_admin": nil})},
		{"create members", authz.EventsExportCreate, members,
			with(with(all(fb), hiddenMembers), map[string]error{"club_admin": nil})},
		{"create university", authz.EventsExportCreate, uniEvent,
			with(with(all(fb), hiddenUni), map[string]error{"university_rep": nil})},
		{"list public", authz.EventsExportList, eventA, with(all(fb), map[string]error{"club_admin": nil, "dpo": nil})},
		{"list members", authz.EventsExportList, members,
			with(with(all(fb), hiddenMembers), map[string]error{"club_admin": nil, "dpo": nil})},
		{"list university", authz.EventsExportList, uniEvent,
			with(with(all(fb), hiddenUni), map[string]error{"university_rep": nil, "dpo": nil})},
		{"download: the event pre-check", authz.EventsExportDownload, eventA, with(all(fb), map[string]error{"club_admin": nil})},
		{"download own export", authz.EventsExportDownload, exportRes("club_a", "public", "club_admin", staffOfEventA),
			with(all(fb), map[string]error{"club_admin": nil})},
		{"download another manager's export", authz.EventsExportDownload,
			exportRes("club_a", "public", "event_manager_a", staffOfEventA), all(fb)},
		{"download own export of a members event", authz.EventsExportDownload,
			exportRes("club_a", "members", "club_admin", staffOfEventA),
			with(with(all(fb), hiddenMembers), map[string]error{"club_admin": nil})},
		{"download university export", authz.EventsExportDownload, exportRes("uni_a", "public", "university_rep", nil),
			with(all(fb), map[string]error{"university_rep": nil})},
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
	p := authztest.Personas()[0]
	// k-po: a platform_admin of the platform org exports its events' data.
	pad := persona(t, "platform_admin")
	pad.Memberships = append(pad.Memberships, authz.Membership{OrgID: authztest.ID("platform_org"), OrgKind: authz.OrgPlatform,
		Role: authz.MemberStaff})
	for _, act := range []authz.Action{authz.EventsExportCreate, authz.EventsExportList, authz.EventsExportDownload} {
		wantErr(t, authz.Can(ctx, pad, act, eventRes("platform_org", "published", "public", "", nil)(p)), nil, false)
	}
	platformExport := exportRes("platform_org", "public", "platform_admin", nil)(p)
	wantErr(t, authz.Can(ctx, pad, authz.EventsExportDownload, platformExport), nil, false)
	// career_staff exports its university's events; university_rep needs
	// its MFA session, and so does the DPO for the list.
	cs := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(),
		Memberships: []authz.Membership{{OrgID: authztest.ID("uni_a"), OrgKind: authz.OrgUniversity, Role: authz.MemberCareerStaff}}}
	wantErr(t, authz.Can(ctx, cs, authz.EventsExportCreate, uniEvent(p)), nil, false)
	rep := persona(t, "university_rep")
	rep.MFAVerified = false
	wantErr(t, authz.Can(ctx, rep, authz.EventsExportCreate, uniEvent(p)), authz.ErrMFARequired, false)
	dpo := persona(t, "dpo")
	dpo.MFAVerified = false
	wantErr(t, authz.Can(ctx, dpo, authz.EventsExportList, eventA(p)), authz.ErrMFARequired, false)
	// The DPO oversees the exports but never downloads one (no grant).
	wantErr(t, authz.Can(ctx, persona(t, "dpo"), authz.EventsExportDownload,
		exportRes("club_a", "public", "dpo", staffOfEventA)(p)), fb, false)
	// A minor club admin is no club_admin (Y4).
	minorAdmin := persona(t, "club_admin")
	minorAdmin.IsMinor = true
	wantErr(t, authz.Can(ctx, minorAdmin, authz.EventsExportCreate, eventA(p)), fb, false)
	// The purge is the system's (03 §3.7); it creates nothing.
	sys := authz.SystemActor("events.purge_exports")
	wantErr(t, authz.Can(ctx, sys, authz.EventsExportPurge, authz.Resource{}), nil, false)
	wantErr(t, authz.Can(ctx, sys, authz.EventsExportCreate, authz.Resource{}), fb, false)
	// A draft event's exports are its managers' only; an export without
	// the event's Attrs is invisible (safe default).
	draft := eventRes("club_a", "draft", "public", "", staffOfEventA)
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.EventsExportList, draft(p)), nf, false)
	org := authztest.ID("club_a")
	owner := persona(t, "club_admin").UserID
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.EventsExportDownload,
		authz.Resource{Type: authz.TypeEventExport, OrgID: &org, OwnerUserID: &owner, Attrs: map[string]string{}}), nf, false)
}
