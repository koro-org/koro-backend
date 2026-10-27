package authz

import "uuid"

// TypeFeed is the reader's feed (feed.feed.read): no org, no owner, no
// Attrs. Declared by the feed policy (Policy.Types); the visibility of each
// listed post is the store's filter (03 §3.6).
const TypeFeed = "feed"

// feedPolicy is the policy of the feed module (03 §5.7). Only tickets of
// the feed module edit this file (BE-001 R5). Written by BE-027 for its 9
// actions, extended by BE-029 for the reader's 8:
//
//	feed.feed.read            every signed-in user (✓; k15 is the store's filter)
//	feed.post.read            P (post; 404 when the post is not visible)
//	feed.post.list_by_org     P (org page; the org's public profile must be visible)
//	feed.post.list_org_manage S: club_admin, university_rep, career_staff, recruiter, company_admin,
//	                            company_owner; platform_admin k-po
//	feed.post.create          S: as list_org_manage (org management surface)
//	feed.post.update          S: as list_org_manage (post; 404 when invisible)
//	feed.post.publish         S: as list_org_manage
//	feed.post.delete          S: as list_org_manage
//	feed.post.stats           S: as list_org_manage
//	feed.reaction.manage      every signed-in user (✓) on a post they see (404 otherwise)
//	feed.save.manage          as reaction.manage
//	feed.hide.create          as reaction.manage
//	feed.follow.manage        every signed-in user (✓) on an org whose profile they see
//	feed.mute.manage          as follow.manage
//	feed.save.list_own        O (feed resource with OwnerUserID = the caller)
//	feed.follow.list_own      O
//	feed.mute.list_own        O
//
// by BE-145 for undoing a hide (Q-35):
//
//	feed.hide.delete          as reaction.manage (unhidePost: on a post the reader no longer sees
//	                          the service still removes their own row, as unlike/unsave)
//	feed.hide.list_own        O
//
// and by BE-032 for the targeted post, the reports and moderation:
//
//	feed.post.create_targeted   S: as feed.post.create (k16, the targeting scope, is outreach.Compile's: 422)
//	feed.report.create          every signed-in user (✓) on a post they see / an org whose profile they see
//	feed.report.list            mod, pad (admin surface: no platform role → 403)
//	feed.report.read            mod, pad (admin surface)
//	feed.report.resolve         mod, pad (admin surface; on the report: not the reporter, Y6's spirit)
//	feed.review_queue.list      mod, pad (admin surface)
//	feed.post.moderate          mod, pad (admin surface; on the post: not its author nor a writer of its org)
//	feed.org.moderate           mod (k17: no suspend_org, no change of a suspended org), pad (admin surface;
//	                            on the org: not a member of it)
//	feed.moderation_action.list mod, pad (admin surface)
//
// The moderation endpoints authorise twice: first the admin surface
// (03 §3.6: every user sees it, a user without a platform role gets 403
// before anything is read), then the target they decide on, where the
// conflict-of-interest rules apply (a moderator does not decide on their
// own work: their post, their org's post, their org, their report).
//
// Reader interactions (BE-029): a like, a save or a hide addresses a post
// (/posts/{post_id}/..., resource surface): the post's visibility is
// postVisible, so an invisible post is 404 and a minor cannot like a
// commercial post. A follow or a mute addresses an org (/me/follows/
// {org_id}): its public profile must be visible (orgProfileVisible). The
// lists are the caller's own (feed resource, OwnerUserID). Following a
// sensitive club is the reader's own choice and reveals nothing to anyone
// else (02 §12.7: the org sees a count at most); what feed never does is
// create one automatically from a sensitive membership (the service).
//
// Resources (03 §3.5): post (OrgID = the author org; Attrs status,
// audience_kind, audience_match, commercial, follows_author), org
// (createPost and listOrgPostsManage address /orgs/{org_id}/posts, the
// org's management surface: its active members and every platform role
// see it, everyone else gets 404; listPosts?org_id= is the org's public
// page: its public profile visibility) and feed.
//
// Post visibility (03 §3.6 "Gönderi"): a published post is visible to its
// author org's writers, to moderators and platform admins, and to a
// reader whose keys match its audience (audience_match), who is not a
// minor when it is commercial (k15) and who follows the author org when it
// is a followers post (follows_author; the service counts a member of the
// author org as a follower). An anonymous reader sees public,
// non-commercial posts only (T-57). A draft, a post in review and a hidden
// post are visible to the author org's writers, moderators and platform
// admins; a removed post to moderators and platform admins only (the
// author org keeps its status in its management list). /posts/{post_id} is
// a resource surface (03 §7.6): an invisible post is 404, a visible one
// without the role 403.
//
// career_staff (as in eventsPolicy): "salt içerik/etkinlik yetkisi" (03
// §1.2) — posts are content, so it holds the uni cells of feed (no MFA,
// k-mfa). k-po (H-39, T-190): platform_admin writes for the platform org
// only (platformOrg of policy_files.go). The status machine, the org
// status (03 §2.3) and the feed trust level are the service's.
func feedPolicy() Policy {
	return Policy{
		Types: map[string][]string{TypeFeed: nil},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case FeedFeedRead:
				return r.Type == TypeFeed && a.IsUser()
			case FeedPostListByOrg:
				return r.Type == TypeOrg && orgProfileVisible(a, r)
			case FeedPostListOrgManage, FeedPostCreate:
				return r.Type == TypeOrg && OrgSurfaceVisible(a, r.OrgID)
			case FeedPostRead, FeedPostUpdate, FeedPostPublish, FeedPostDelete, FeedPostStats,
				FeedReactionManage, FeedSaveManage, FeedHideCreate, FeedHideDelete:
				return r.Type == TypePost && postVisible(a, r)
			case FeedFollowManage, FeedMuteManage:
				return r.Type == TypeOrg && orgProfileVisible(a, r)
			case FeedSaveListOwn, FeedHideListOwn, FeedFollowListOwn, FeedMuteListOwn:
				return r.Type == TypeFeed && a.Owns(r)
			case FeedPostCreateTargeted:
				return r.Type == TypeOrg && OrgSurfaceVisible(a, r.OrgID)
			case FeedReportCreate:
				switch r.Type {
				case TypePost:
					return postVisible(a, r)
				case TypeOrg:
					return orgProfileVisible(a, r)
				}
				return false
			case FeedReportList, FeedReportRead, FeedReviewQueueList, FeedModerationActionList:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			case FeedReportResolve, FeedPostModerate, FeedOrgModerate:
				// The admin surface, then the decided target: an admin
				// resource is no secret (403, never 404).
				return AdminSurfaceVisible(a) && (r.Type == TypeAdminSurface ||
					act == FeedReportResolve && r.Type == TypeContentReport ||
					act == FeedPostModerate && r.Type == TypePost || act == FeedOrgModerate && r.Type == TypeOrg)
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			switch act {
			case FeedFeedRead, FeedReactionManage, FeedSaveManage, FeedHideCreate, FeedHideDelete, FeedFollowManage,
				FeedMuteManage:
				// Every user is either a verified student (minor or not) or
				// not: the three paths cover every signed-in actor.
				return []Grant{{Role: RoleUserUnverified}, {Role: RoleStudent}, {Role: RoleStudentMinor}}
			case FeedSaveListOwn, FeedHideListOwn, FeedFollowListOwn, FeedMuteListOwn:
				return []Grant{{Role: RoleSelf}}
			case FeedPostRead, FeedPostListByOrg:
				return []Grant{{Role: RolePublic}}
			case FeedPostListOrgManage, FeedPostCreate, FeedPostUpdate, FeedPostPublish, FeedPostDelete, FeedPostStats:
				gs := postWriterGrants(a, r.OrgID)
				if platformOrg(a, r.OrgID) { // k-po
					gs = append(gs, Grant{Role: RolePlatformAdmin})
				}
				return gs
			case FeedPostCreateTargeted:
				gs := postWriterGrants(a, r.OrgID)
				if platformOrg(a, r.OrgID) { // k-po
					gs = append(gs, Grant{Role: RolePlatformAdmin})
				}
				return gs
			case FeedReportCreate:
				return []Grant{{Role: RoleUserUnverified}, {Role: RoleStudent}, {Role: RoleStudentMinor}}
			case FeedReportList, FeedReportRead, FeedReviewQueueList, FeedModerationActionList:
				return moderators
			case FeedReportResolve:
				if r.Type == TypeContentReport && a.Owns(r) {
					return nil // nobody resolves their own report
				}
				return moderators
			case FeedPostModerate:
				if r.Type == TypePost && (a.Owns(r) || postWriter(a, r)) {
					return nil // nobody moderates their own or their org's post
				}
				return moderators
			case FeedOrgModerate:
				return orgModerationGrants(a, r)
			}
			return nil
		},
	}
}

// moderators are the grants of the moderation actions (03 §5.7 mod, pad;
// both MFA roles, k-mfa).
var moderators = []Grant{{Role: RoleModerator}, {Role: RolePlatformAdmin}}

// orgModerationGrants is feed.org.moderate on the admin surface or on the
// decided org (Attrs moderation_action, trust_level): nobody moderates an
// org they belong to; k17 — a moderator neither suspends an org nor
// changes a suspended one (lifting a suspension is a platform admin's).
func orgModerationGrants(a Actor, r Resource) []Grant {
	if r.Type != TypeOrg {
		return moderators
	}
	if r.OrgID != nil && a.MemberOf(*r.OrgID) {
		return nil
	}
	if r.Attr("moderation_action") == "suspend_org" || r.Attr("trust_level") == "suspended" &&
		r.Attr("moderation_action") != "warn_org" {
		return []Grant{{Role: RolePlatformAdmin}}
	}
	return moderators
}

// postWriters are the org roles that write an org's posts (03 §5.7 S cells
// of cla, uni, cmm, cma; career_staff, see feedPolicy).
var postWriters = []struct {
	member MemberRole
	role   Role
}{
	{MemberClubAdmin, RoleClubAdmin}, {MemberUniversityRep, RoleUniversityRep}, {MemberCareerStaff, RoleCareerStaff},
	{MemberRecruiter, RoleCompanyMember}, {MemberCompanyAdmin, RoleCompanyAdmin}, {MemberCompanyOwner, RoleCompanyOwner},
}

// postWriterGrants returns the S grants the actor holds in the author org.
func postWriterGrants(a Actor, orgID *uuid.UUID) []Grant {
	if orgID == nil {
		return nil
	}
	var gs []Grant
	for _, w := range postWriters {
		if a.HasOrgRole(*orgID, w.member) {
			gs = append(gs, Grant{Role: w.role})
		}
	}
	return gs
}

// postWriter reports whether a writes the posts of the author org of r.
func postWriter(a Actor, r Resource) bool {
	return len(postWriterGrants(a, r.OrgID)) > 0 || platformOrg(a, r.OrgID)
}

// postVisible is 03 §3.6 "Gönderi".
func postVisible(a Actor, r Resource) bool {
	staff := a.HasPlatformRole(PlatformModerator) || a.HasPlatformRole(PlatformAdmin)
	switch r.Attr(AttrStatus) {
	case "published":
		if staff || postWriter(a, r) {
			return true
		}
		if !a.IsUser() {
			return r.Attr("audience_kind") == "public" && !r.Flag("commercial")
		}
		switch {
		case !r.Flag("audience_match"), r.Flag("commercial") && a.IsMinor:
			return false
		case r.Attr("audience_kind") == "followers":
			return r.Flag("follows_author")
		}
		return true
	case "draft", "pending_review", "hidden":
		return staff || postWriter(a, r)
	case "removed":
		return staff
	}
	return false
}
