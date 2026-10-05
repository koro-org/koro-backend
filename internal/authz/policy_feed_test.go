package authz_test

import (
	"slices"
	"strconv"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// postRes is the post resource the feed service builds (03 §3.5; 06
// H-27): the author org, the status, the audience, the reader's audience
// match, the commercial flag and whether the reader follows the author
// (the service counts a member of the author org as a follower).
func postRes(org, status, audience string, commercial bool) func(authztest.Persona) authz.Resource {
	return func(p authztest.Persona) authz.Resource {
		id := authztest.ID(org)
		follows := slices.ContainsFunc(p.Actor.Memberships, func(m authz.Membership) bool { return m.OrgID == id })
		return authz.Resource{Type: authz.TypePost, ID: authztest.ID("post_" + org), OrgID: &id, Attrs: map[string]string{
			authz.AttrStatus: status, "audience_kind": audience, "audience_match": "true",
			"commercial": strconv.FormatBool(commercial), "follows_author": strconv.FormatBool(follows),
		}}
	}
}

// ownFeed is the reader's own list (feed resource owned by the persona);
// othersFeed is someone else's.
func ownFeed(p authztest.Persona) authz.Resource {
	me := p.Actor.UserID
	return authz.Resource{Type: authz.TypeFeed, OwnerUserID: &me}
}

func othersFeed(authztest.Persona) authz.Resource {
	other := uuid.NewV7()
	return authz.Resource{Type: authz.TypeFeed, OwnerUserID: &other}
}

// TestFeedPolicy_Matrix pins 03 §5.7 (BE-027's 9 and BE-029's 8 actions) for the 18
// personas; testdata/authz/feed.yaml checks the same through the API.
func TestFeedPolicy_Matrix(t *testing.T) {
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
	// hidden: everyone 404 except anonymous, whom the core refuses first on
	// a non-public action (403; 401 over HTTP).
	hidden := func() map[string]error { return with(all(nf), map[string]error{"anonymous": fb}) }
	staff := []string{"moderator", "support", "dpo", "platform_admin", "finance_admin", "talent_auditor"}
	staffAre := func(m map[string]error, err error, names ...string) map[string]error {
		if len(names) == 0 {
			names = staff
		}
		for _, s := range names {
			m[s] = err
		}
		return m
	}
	// BE-032: the moderation endpoints are the admin surface (403 without a
	// platform role, 401 anonymous) and its decided targets.
	modOnly := func() map[string]error {
		return with(all(fb), map[string]error{"moderator": nil, "platform_admin": nil})
	}
	adminRes := func(authztest.Persona) authz.Resource { return authz.Resource{Type: authz.TypeAdminSurface} }
	pub := postRes("club_a", "published", "public", false)
	ad := postRes("company_a", "published", "public", true)
	followers := postRes("club_a", "published", "followers", false)
	draft := postRes("club_a", "draft", "public", false)
	review := postRes("company_a", "pending_review", "public", true)
	removed := postRes("club_a", "removed", "public", false)
	for _, tc := range []struct {
		name string
		act  authz.Action
		res  func(authztest.Persona) authz.Resource
		want map[string]error
	}{
		{"feed read", authz.FeedFeedRead, func(authztest.Persona) authz.Resource { return authz.Resource{Type: authz.TypeFeed} },
			with(all(nil), map[string]error{"anonymous": fb})},
		{"read public", authz.FeedPostRead, pub, all(nil)},
		{"read commercial (k15, T-57)", authz.FeedPostRead, ad, with(all(nil), map[string]error{"anonymous": nf, "student_minor": nf})},
		{"read followers", authz.FeedPostRead, followers, with(all(nf), map[string]error{
			"club_member": nil, "club_admin": nil, "moderator": nil, "platform_admin": nil})},
		{"read draft", authz.FeedPostRead, draft, with(all(nf), map[string]error{"club_admin": nil, "moderator": nil, "platform_admin": nil})},
		{"read pending_review", authz.FeedPostRead, review, with(all(nf), map[string]error{
			"company_member": nil, "company_admin": nil, "moderator": nil, "platform_admin": nil})},
		{"read removed", authz.FeedPostRead, removed, with(all(nf), map[string]error{"moderator": nil, "platform_admin": nil})},
		{"list_by_org", authz.FeedPostListByOrg, func(authztest.Persona) authz.Resource { return orgRes("club_a", authz.OrgClub, "active") },
			all(nil)},
		{"list_by_org pending company", authz.FeedPostListByOrg,
			func(authztest.Persona) authz.Resource { return orgRes("company_a", authz.OrgCompany, "pending_review") },
			with(all(nf), map[string]error{"company_member": nil, "company_admin": nil, "moderator": nil, "support": nil,
				"platform_admin": nil})},
		{"list_org_manage club_a", authz.FeedPostListOrgManage, orgSurface("club_a", authz.OrgClub),
			staffAre(with(hidden(), map[string]error{"club_member": fb, "club_admin": nil}), fb)},
		{"create company_a", authz.FeedPostCreate, orgSurface("company_a", authz.OrgCompany),
			staffAre(with(hidden(), map[string]error{"company_member": nil, "company_admin": nil}), fb)},
		{"create uni_a", authz.FeedPostCreate, orgSurface("uni_a", authz.OrgUniversity),
			staffAre(with(hidden(), map[string]error{"university_rep": nil}), fb)},
		{"update public", authz.FeedPostUpdate, pub, with(all(fb), map[string]error{"club_admin": nil})},
		{"update draft", authz.FeedPostUpdate, draft,
			with(hidden(), map[string]error{"club_admin": nil, "moderator": fb, "platform_admin": fb})},
		{"publish pending company", authz.FeedPostPublish, review,
			with(hidden(), map[string]error{"company_member": nil, "company_admin": nil, "moderator": fb, "platform_admin": fb})},
		{"delete followers", authz.FeedPostDelete, followers,
			with(hidden(), map[string]error{"club_member": fb, "club_admin": nil, "moderator": fb, "platform_admin": fb})},
		{"stats commercial", authz.FeedPostStats, ad,
			with(all(fb), map[string]error{"student_minor": nf, "company_member": nil, "company_admin": nil})},
		// BE-029: the reader's interactions follow the post's visibility.
		{"like public", authz.FeedReactionManage, pub, with(all(nil), map[string]error{"anonymous": fb})},
		{"save commercial (k15)", authz.FeedSaveManage, ad, with(all(nil), map[string]error{"anonymous": fb, "student_minor": nf})},
		{"hide followers", authz.FeedHideCreate, followers, with(hidden(), map[string]error{
			"club_member": nil, "club_admin": nil, "moderator": nil, "platform_admin": nil})},
		{"like draft (the service refuses: 409)", authz.FeedReactionManage, draft, with(hidden(), map[string]error{
			"club_admin": nil, "moderator": nil, "platform_admin": nil})},
		{"like removed", authz.FeedReactionManage, removed, with(hidden(), map[string]error{"moderator": nil, "platform_admin": nil})},
		{"follow club_a", authz.FeedFollowManage, func(authztest.Persona) authz.Resource { return orgRes("club_a", authz.OrgClub, "active") },
			with(all(nil), map[string]error{"anonymous": fb})},
		{"mute pending company", authz.FeedMuteManage,
			func(authztest.Persona) authz.Resource { return orgRes("company_a", authz.OrgCompany, "pending_review") },
			with(hidden(), map[string]error{"company_member": nil, "company_admin": nil, "moderator": nil, "support": nil,
				"platform_admin": nil})},
		{"list own saves", authz.FeedSaveListOwn, ownFeed, with(all(nil), map[string]error{"anonymous": fb})},
		{"list own follows", authz.FeedFollowListOwn, ownFeed, with(all(nil), map[string]error{"anonymous": fb})},
		{"list own mutes", authz.FeedMuteListOwn, ownFeed, with(all(nil), map[string]error{"anonymous": fb})},
		{"list another user's mutes", authz.FeedMuteListOwn, othersFeed, hidden()},
		// BE-032: targeted posts, reports and moderation.
		{"create_targeted club_a", authz.FeedPostCreateTargeted, orgSurface("club_a", authz.OrgClub),
			staffAre(with(hidden(), map[string]error{"club_member": fb, "club_admin": nil}), fb)},
		{"create_targeted company_a", authz.FeedPostCreateTargeted, orgSurface("company_a", authz.OrgCompany),
			staffAre(with(hidden(), map[string]error{"company_member": nil, "company_admin": nil}), fb)},
		{"create_targeted uni_a", authz.FeedPostCreateTargeted, orgSurface("uni_a", authz.OrgUniversity),
			staffAre(with(hidden(), map[string]error{"university_rep": nil}), fb)},
		{"report public post", authz.FeedReportCreate, pub, with(all(nil), map[string]error{"anonymous": fb})},
		{"report commercial post (k15)", authz.FeedReportCreate, ad,
			with(all(nil), map[string]error{"anonymous": fb, "student_minor": nf})},
		{"report draft (the service refuses: 409)", authz.FeedReportCreate, draft,
			with(hidden(), map[string]error{"club_admin": nil, "moderator": nil, "platform_admin": nil})},
		{"report org", authz.FeedReportCreate, func(authztest.Persona) authz.Resource { return orgRes("company_a", authz.OrgCompany, "active") },
			with(all(nil), map[string]error{"anonymous": fb})},
		{"report list", authz.FeedReportList, adminRes, modOnly()},
		{"report read", authz.FeedReportRead, adminRes, modOnly()},
		{"report resolve (surface)", authz.FeedReportResolve, adminRes, modOnly()},
		{"report resolve (report of someone else)", authz.FeedReportResolve, reportOf(uuid.NewV7()), modOnly()},
		{"review queue", authz.FeedReviewQueueList, adminRes, modOnly()},
		{"moderation history", authz.FeedModerationActionList, adminRes, modOnly()},
		{"moderate post (surface)", authz.FeedPostModerate, adminRes, modOnly()},
		{"moderate post (club_a)", authz.FeedPostModerate, moderated(pub), modOnly()},
		{"moderate post in review", authz.FeedPostModerate, moderated(review), modOnly()},
		{"moderate org (surface)", authz.FeedOrgModerate, adminRes, modOnly()},
		{"warn company_a", authz.FeedOrgModerate, orgDecision("company_a", authz.OrgCompany, "warn_org", "new"), modOnly()},
		{"restrict company_a", authz.FeedOrgModerate, orgDecision("company_a", authz.OrgCompany, "restrict_org", "trusted"), modOnly()},
		{"suspend company_a (k17)", authz.FeedOrgModerate, orgDecision("company_a", authz.OrgCompany, "suspend_org", "new"),
			with(all(fb), map[string]error{"platform_admin": nil})},
		{"restrict a suspended org (k17)", authz.FeedOrgModerate,
			orgDecision("company_a", authz.OrgCompany, "restrict_org", "suspended"), with(all(fb), map[string]error{"platform_admin": nil})},
		{"warn a suspended org", authz.FeedOrgModerate, orgDecision("company_a", authz.OrgCompany, "warn_org", "suspended"), modOnly()},
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

func TestFeedPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p := authztest.Personas()[0]
	// k-po: a platform_admin who belongs to the platform org writes its posts.
	pad := persona(t, "platform_admin")
	pad.Memberships = append(pad.Memberships, authz.Membership{OrgID: authztest.ID("platform_org"), OrgKind: authz.OrgPlatform,
		Role: authz.MemberStaff})
	platformDraft := postRes("platform_org", "draft", "public", false)
	for _, act := range []authz.Action{authz.FeedPostUpdate, authz.FeedPostPublish, authz.FeedPostDelete, authz.FeedPostStats} {
		wantErr(t, authz.Can(ctx, pad, act, platformDraft(p)), nil, false)
	}
	for _, act := range []authz.Action{authz.FeedPostCreate, authz.FeedPostListOrgManage} {
		wantErr(t, authz.Can(ctx, pad, act, orgRes("platform_org", authz.OrgPlatform, "active")), nil, false)
	}
	// career_staff writes its university's posts, without MFA (content, 03 §1.2).
	cs := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(),
		Memberships: []authz.Membership{{OrgID: authztest.ID("uni_a"), OrgKind: authz.OrgUniversity, Role: authz.MemberCareerStaff}}}
	uniDraft := postRes("uni_a", "draft", "public", false)
	for _, act := range []authz.Action{authz.FeedPostUpdate, authz.FeedPostPublish, authz.FeedPostDelete} {
		wantErr(t, authz.Can(ctx, cs, act, uniDraft(p)), nil, false)
	}
	// university_rep and company_admin need their MFA session (k-mfa);
	// company_member (recruiter) does not.
	rep := persona(t, "university_rep")
	rep.MFAVerified = false
	wantErr(t, authz.Can(ctx, rep, authz.FeedPostPublish, uniDraft(p)), authz.ErrMFARequired, false)
	cma := persona(t, "company_admin")
	cma.MFAVerified = false
	companyDraft := postRes("company_a", "draft", "public", true)
	wantErr(t, authz.Can(ctx, cma, authz.FeedPostPublish, companyDraft(p)), authz.ErrMFARequired, false)
	cmm := persona(t, "company_member")
	cmm.MFAVerified = false
	wantErr(t, authz.Can(ctx, cmm, authz.FeedPostPublish, companyDraft(p)), nil, false)
	// A segment post a reader's keys do not match is invisible; its writers see it.
	seg := postRes("club_a", "published", "segment", false)(p)
	seg.Attrs["audience_match"] = "false"
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.FeedPostRead, seg), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.FeedPostRead, seg), nil, false)
	// A hidden post is its writers' and the moderators' only.
	hiddenPost := postRes("club_a", "hidden", "public", false)
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.FeedPostRead, hiddenPost(p)), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.FeedPostUpdate, hiddenPost(p)), nil, false)
	// An unknown status hides the post (safe default).
	wantErr(t, authz.Can(ctx, authz.Anonymous(), authz.FeedPostRead, postRes("club_a", "archived", "public", false)(p)),
		authz.ErrNotFound, false)
	// BE-032: nobody decides on their own work. A moderator who writes for
	// club_a does not moderate its posts; the author of a post does not
	// moderate it; a platform admin who belongs to company_a does not
	// moderate it; the reporter does not resolve their own report.
	modClub := persona(t, "moderator")
	modClub.Memberships = append(modClub.Memberships, authz.Membership{OrgID: authztest.ID("club_a"), OrgKind: authz.OrgClub,
		Role: authz.MemberClubAdmin})
	pubA := moderated(postRes("club_a", "published", "public", false))(p)
	wantErr(t, authz.Can(ctx, modClub, authz.FeedPostModerate, pubA), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, modClub, authz.FeedPostModerate, authz.Resource{Type: authz.TypeAdminSurface}), nil, false)
	mod := persona(t, "moderator")
	own := postRes("club_b", "published", "public", false)(p)
	me := mod.UserID
	own.OwnerUserID = &me
	wantErr(t, authz.Can(ctx, mod, authz.FeedPostModerate, own), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, mod, authz.FeedPostModerate, pubA), nil, false)
	padCo := persona(t, "platform_admin")
	padCo.Memberships = append(padCo.Memberships, authz.Membership{OrgID: authztest.ID("company_a"), OrgKind: authz.OrgCompany,
		Role: authz.MemberCompanyAdmin})
	coSuspend := orgDecision("company_a", authz.OrgCompany, "suspend_org", "new")(p)
	wantErr(t, authz.Can(ctx, padCo, authz.FeedOrgModerate, coSuspend), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.FeedOrgModerate, coSuspend), nil, false)
	wantErr(t, authz.Can(ctx, mod, authz.FeedReportResolve, reportOf(mod.UserID)(p)), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.FeedReportResolve, reportOf(mod.UserID)(p)), nil, false)
	// The moderation endpoints are k-mfa (mod, pad).
	modNoMFA := persona(t, "moderator")
	modNoMFA.MFAVerified = false
	wantErr(t, authz.Can(ctx, modNoMFA, authz.FeedReportList, authz.Resource{Type: authz.TypeAdminSurface}), authz.ErrMFARequired, false)
	// k-po: the platform org's targeted posts are its platform admin's.
	wantErr(t, authz.Can(ctx, pad, authz.FeedPostCreateTargeted, orgRes("platform_org", authz.OrgPlatform, "active")), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.FeedPostCreateTargeted,
		orgRes("company_a", authz.OrgCompany, "active")), authz.ErrForbidden, false)
	// Missing Attrs are a programmer error (deny).
	org := authztest.ID("club_a")
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.FeedPostUpdate,
		authz.Resource{Type: authz.TypePost, OrgID: &org, Attrs: map[string]string{authz.AttrStatus: "draft"}}), authz.ErrForbidden, true)
}

// moderated is the post resource a moderation decision builds: the post
// with its author (a third party here) as owner.
func moderated(res func(authztest.Persona) authz.Resource) func(authztest.Persona) authz.Resource {
	return func(p authztest.Persona) authz.Resource {
		r := res(p)
		author := uuid.NewV7()
		r.OwnerUserID = &author
		return r
	}
}

// orgDecision is the org resource of adminModerateOrg (Attrs
// moderation_action and trust_level).
func orgDecision(name string, kind authz.OrgKind, action, trust string) func(authztest.Persona) authz.Resource {
	return func(authztest.Persona) authz.Resource {
		r := orgRes(name, kind, "active")
		r.Attrs["moderation_action"] = action
		r.Attrs["trust_level"] = trust
		return r
	}
}

// reportOf is a content report filed by reporter.
func reportOf(reporter uuid.UUID) func(authztest.Persona) authz.Resource {
	return func(authztest.Persona) authz.Resource {
		return authz.Resource{Type: authz.TypeContentReport, ID: uuid.NewV7(), OwnerUserID: &reporter}
	}
}
