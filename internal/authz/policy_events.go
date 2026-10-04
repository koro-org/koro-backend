package authz

import "uuid"

// TypeEventCatalog is the public event list (events.event.list): no org,
// no owner, no Attrs. Declared by the events policy (Policy.Types); the
// visibility of each listed event is the store's filter (03 §3.6).
const TypeEventCatalog = "event_catalog"

// eventsPolicy is the policy of the events module (03 §5.6). Only tickets of
// the events module edit this file (BE-001 R5). Written by BE-024 for its
// 10 actions:
//
//	events.event.list        P (event_catalog; the store filters by visibility)
//	events.event.read        P (event; 404 when the event is not visible)
//	events.event.list_org    S: club_admin, university_rep, career_staff; moderator, support,
//	                           platform_admin (org management surface)
//	events.event.create      S: club_admin, university_rep, career_staff; platform_admin k-po
//	                           (company ✗, k13: no grant → 403)
//	events.event.update      S: club_admin, university_rep, career_staff; platform_admin
//	events.event.publish     S: club_admin, university_rep, career_staff; platform_admin k-po
//	events.event.cancel      S: club_admin, university_rep, career_staff; platform_admin
//	events.event.delete      S: club_admin, university_rep, career_staff; platform_admin k-po
//	events.staff.list        S: club_admin, university_rep, career_staff; platform_admin k-po
//	events.staff.manage      S: club_admin, university_rep, career_staff; platform_admin k-po
//
// and by BE-026 for the form actions:
//
//	events.form.read         P (event_form; visible when the event is)
//	events.form.manage       S: club_admin, university_rep, career_staff; platform_admin k-po
//	events.form.publish      S: club_admin, university_rep, career_staff; platform_admin k-po
//	events.lint_rule.manage  dpo, platform_admin (admin surface; k-mfa)
//
// Resources (03 §3.5): event (OrgID = the organiser; Attrs status,
// visibility, visibility_university_id, sensitive, event_staff_role),
// event_form (the same Attrs plus event_status: the form is a sub-surface
// of its event), org (createEvent and listOrgEvents address
// /orgs/{org_id}/events, the org's management surface: its active members
// and every platform role see it, everyone else gets 404), event_catalog
// and admin_surface (/admin/form-lint-rules).
//
// Form visibility (03 §3.6 "Etkinlik formu"): the published version is
// visible to everyone who sees the event; the draft schema (and so the
// publication) to the event's management only: a visible event's form
// draft is 403 for the organiser's other active members and every platform
// role without the grant, 404 for everyone else (another org's form is
// 404, BE-026 reading of the S cell, 03 §5.0).
//
// Event visibility (03 §3.6): a published, completed or cancelled event is
// visible to everyone when public; to verified students of
// visibility_university_id, the organiser's active members and the event's
// staff when university; to the organiser's active members and the staff
// when members. A draft is visible only to the organiser's club_admin and
// university_rep (career_staff included, see below). Every platform role
// sees every event (403 without a grant). /events/{event_id}/... is a
// resource surface (03 §7.6): an invisible event is 404, a visible one
// without the role 403.
//
// career_staff (BE-024 reading of 03 §1.1–1.2 and T-59): career_staff takes
// the university_rep cells except member/invitation management and
// offering approval; it "does content and events", so it holds the uni
// cells of events (no MFA, k-mfa). k-po (H-39, T-190): platform_admin acts
// for the platform org only, i.e. an org the actor belongs to with kind
// platform (platformOrg of policy_files.go). The status rules (draft-only
// delete, publish requirements) and the org status (03 §2.3) are the
// service's; they are not authz codes.
func eventsPolicy() Policy {
	return Policy{
		Types: map[string][]string{TypeEventCatalog: nil},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case EventsEventList:
				return r.Type == TypeEventCatalog
			case EventsEventCreate, EventsEventListOrg:
				return r.Type == TypeOrg && OrgSurfaceVisible(a, r.OrgID)
			case EventsFormRead:
				return r.Type == TypeEventForm && eventVisibleAt(a, r, r.Attr("event_status"))
			case EventsFormManage, EventsFormPublish:
				return r.Type == TypeEventForm && eventVisibleAt(a, r, r.Attr("event_status")) && OrgSurfaceVisible(a, r.OrgID)
			case EventsLintRuleManage:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			}
			return r.Type == TypeEvent && eventVisible(a, r)
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			switch act {
			case EventsEventList, EventsEventRead, EventsFormRead:
				return []Grant{{Role: RolePublic}}
			case EventsLintRuleManage:
				return []Grant{{Role: RoleDPO}, {Role: RolePlatformAdmin}}
			case EventsEventListOrg:
				return append(eventOrgGrants(a, r.OrgID),
					Grant{Role: RoleModerator}, Grant{Role: RoleSupport}, Grant{Role: RolePlatformAdmin})
			case EventsEventUpdate, EventsEventCancel:
				return append(eventOrgGrants(a, r.OrgID), Grant{Role: RolePlatformAdmin})
			case EventsEventCreate, EventsEventPublish, EventsEventDelete, EventsStaffList, EventsStaffManage,
				EventsFormManage, EventsFormPublish:
				gs := eventOrgGrants(a, r.OrgID)
				if platformOrg(a, r.OrgID) { // k-po
					gs = append(gs, Grant{Role: RolePlatformAdmin})
				}
				return gs
			}
			return nil // the actions of BE-028, BE-031, BE-050: not written yet
		},
	}
}

// eventManagers are the org roles that manage an organiser's events (cell
// S of club_admin and university_rep; career_staff, see eventsPolicy).
var eventManagers = []struct {
	member MemberRole
	role   Role
}{{MemberClubAdmin, RoleClubAdmin}, {MemberUniversityRep, RoleUniversityRep}, {MemberCareerStaff, RoleCareerStaff}}

// eventOrgGrants returns the S grants the actor holds in the organiser org.
func eventOrgGrants(a Actor, orgID *uuid.UUID) []Grant {
	if orgID == nil {
		return nil
	}
	var gs []Grant
	for _, m := range eventManagers {
		if a.HasOrgRole(*orgID, m.member) {
			gs = append(gs, Grant{Role: m.role})
		}
	}
	return gs
}

// eventVisible is 03 §3.6 "Etkinlik".
func eventVisible(a Actor, r Resource) bool { return eventVisibleAt(a, r, r.Attr(AttrStatus)) }

// eventVisibleAt is eventVisible with the event's status given (an
// event_form resource carries it as event_status).
func eventVisibleAt(a Actor, r Resource, status string) bool {
	if a.HasAnyPlatformRole() {
		return true
	}
	org := r.OrgID
	member := org != nil && a.MemberOf(*org)
	switch status {
	case "draft":
		return org != nil && a.HasOrgRole(*org, MemberClubAdmin, MemberUniversityRep, MemberCareerStaff)
	case "published", "completed", "cancelled":
	default:
		return false
	}
	staff := r.Attr(AttrEventStaffRole) == "scanner" || r.Attr(AttrEventStaffRole) == "manager"
	switch r.Attr("visibility") {
	case "public":
		return true
	case "university":
		uni := r.Attr("visibility_university_id")
		student := a.IsVerifiedStudent() && a.UniversityID != nil && uni != "" && a.UniversityID.String() == uni
		return student || member || staff
	case "members":
		return member || staff
	}
	return false
}
