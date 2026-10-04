package authz

import "uuid"

// TypeFeed is the reader's feed (feed.feed.read): no org, no owner, no
// Attrs. Declared by the feed policy (Policy.Types); the visibility of each
// listed post is the store's filter (03 §3.6).
const TypeFeed = "feed"

// feedPolicy is the policy of the feed module (03 §5.7). Only tickets of
// the feed module edit this file (BE-001 R5). Written by BE-027 for its 9
// actions and extended by BE-029 for the reader's 8:
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
// The other 9 feed actions belong to BE-032 (nil grants: 403).
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
				FeedReactionManage, FeedSaveManage, FeedHideCreate:
				return r.Type == TypePost && postVisible(a, r)
			case FeedFollowManage, FeedMuteManage:
				return r.Type == TypeOrg && orgProfileVisible(a, r)
			case FeedSaveListOwn, FeedFollowListOwn, FeedMuteListOwn:
				return r.Type == TypeFeed && a.Owns(r)
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			switch act {
			case FeedFeedRead, FeedReactionManage, FeedSaveManage, FeedHideCreate, FeedFollowManage, FeedMuteManage:
				// Every user is either a verified student (minor or not) or
				// not: the three paths cover every signed-in actor.
				return []Grant{{Role: RoleUserUnverified}, {Role: RoleStudent}, {Role: RoleStudentMinor}}
			case FeedSaveListOwn, FeedFollowListOwn, FeedMuteListOwn:
				return []Grant{{Role: RoleSelf}}
			case FeedPostRead, FeedPostListByOrg:
				return []Grant{{Role: RolePublic}}
			case FeedPostListOrgManage, FeedPostCreate, FeedPostUpdate, FeedPostPublish, FeedPostDelete, FeedPostStats:
				gs := postWriterGrants(a, r.OrgID)
				if platformOrg(a, r.OrgID) { // k-po
					gs = append(gs, Grant{Role: RolePlatformAdmin})
				}
				return gs
			}
			return nil // the actions of BE-032: not written yet
		},
	}
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
