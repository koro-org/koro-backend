package authz_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// companyRequest is the Resource of the company path (03 §3.5; H-27):
// contact_request on the path's company with the student as owner.
func companyRequest(org string, student uuid.UUID) authz.Resource {
	id := authztest.ID(org)
	return authz.Resource{Type: authz.TypeContactRequest, ID: uuid.NewV7(), OrgID: &id, OwnerUserID: &student,
		Attrs: map[string]string{authz.AttrStatus: "pending"}}
}

// ownRequest is the Resource of the /me path: no OrgID, the student as
// owner.
func ownRequest(student uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypeContactRequest, ID: uuid.NewV7(), OwnerUserID: &student,
		Attrs: map[string]string{authz.AttrStatus: "pending"}}
}

// TestMessagingPolicy_CompanyPath pins 03 §5.9 for the company surface:
// cmm S and cma S of the path's company, platform roles and members of the
// org without a company role 403, everyone else 404 (03 §7.6).
func TestMessagingPolicy_CompanyPath(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	staff := map[string]error{
		"anonymous": forbidden, "moderator": forbidden, "support": forbidden, "dpo": forbidden,
		"platform_admin": forbidden, "finance_admin": forbidden, "talent_auditor": forbidden,
	}
	with := func(extra map[string]error) map[string]error {
		out := map[string]error{}
		for k, v := range staff {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	student := authztest.ID("student_granted")
	for _, tc := range []struct {
		org  string
		want map[string]error
	}{
		{"company_a", with(map[string]error{"company_member": nil, "company_admin": nil})},
		{"company_b", with(map[string]error{"other_company_member": nil})},
		{"club_a", with(map[string]error{"club_member": forbidden, "club_admin": forbidden})},
	} {
		for _, act := range []authz.Action{authz.MessagingContactRequestCreate, authz.MessagingContactRequestListSent,
			authz.MessagingContactRequestRead, authz.MessagingContactRequestWithdraw} {
			for _, p := range authztest.Personas() {
				want, ok := tc.want[p.Name]
				if !ok {
					want = notFound
				}
				wantErr(t, authz.Can(ctx, p.Actor, act, companyRequest(tc.org, student)), want, false)
			}
		}
	}
}

// TestMessagingPolicy_StudentPath pins the O cells: every signed-in user
// on their own request and blocks; another user's request or block is not
// found; anonymous is forbidden.
func TestMessagingPolicy_StudentPath(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ownActs := []authz.Action{authz.MessagingContactRequestRead, authz.MessagingContactRequestListReceived,
		authz.MessagingContactRequestRespond}
	blockActs := []authz.Action{authz.MessagingBlockManage, authz.MessagingBlockListOwn}
	other := authztest.ID("student_granted")
	for _, p := range authztest.Personas() {
		me := p.Actor.UserID
		block := authz.Resource{Type: authz.TypeContactBlock, OwnerUserID: &me}
		otherBlock := authz.Resource{Type: authz.TypeContactBlock, OwnerUserID: &other}
		if p.Name == "anonymous" {
			for _, act := range append(ownActs, blockActs...) {
				wantErr(t, authz.Can(ctx, p.Actor, act, ownRequest(other)), authz.ErrForbidden, false)
			}
			continue
		}
		for _, act := range ownActs {
			wantErr(t, authz.Can(ctx, p.Actor, act, ownRequest(me)), nil, false)
			wantErr(t, authz.Can(ctx, p.Actor, act, ownRequest(other)), authz.ErrNotFound, false)
		}
		for _, act := range blockActs {
			wantErr(t, authz.Can(ctx, p.Actor, act, block), nil, false)
			wantErr(t, authz.Can(ctx, p.Actor, act, otherBlock), authz.ErrNotFound, false)
		}
	}
}

func TestMessagingPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	student := authztest.ID("student")

	// company_owner counts as company_admin (S).
	wantErr(t, authz.Can(ctx, persona(t, "company_owner_a"), authz.MessagingContactRequestCreate,
		companyRequest("company_a", student)), nil, false)

	// k-mfa: company_admin needs an MFA session; the recruiter does not (T-59).
	cma := persona(t, "company_admin")
	cma.MFAVerified = false
	wantErr(t, authz.Can(ctx, cma, authz.MessagingContactRequestCreate, companyRequest("company_a", student)),
		authz.ErrMFARequired, false)
	cmm := persona(t, "company_member")
	cmm.MFAVerified = false
	wantErr(t, authz.Can(ctx, cmm, authz.MessagingContactRequestCreate, companyRequest("company_a", student)), nil, false)

	// The student of a request never reads it on the company's surface
	// (the company path is the org's), and a company member never reads a
	// request on the /me path.
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.MessagingContactRequestRead,
		companyRequest("company_a", student)), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "company_member"), authz.MessagingContactRequestRead, ownRequest(student)),
		authz.ErrNotFound, false)

	// Respond and the received list exist on the /me path only: a request
	// with an org is not the student's own surface.
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.MessagingContactRequestRespond,
		companyRequest("company_a", student)), authz.ErrNotFound, false)

	// The mandatory Attrs key (03 §3.5) and the resource types.
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.MessagingContactRequestRespond,
		authz.Resource{Type: authz.TypeContactRequest, OwnerUserID: &student}), authz.ErrForbidden, true)
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.MessagingBlockManage,
		authz.Resource{Type: authz.TypeContactRequest, OwnerUserID: &student, Attrs: map[string]string{authz.AttrStatus: ""}}),
		authz.ErrNotFound, false)
	org := authztest.ID("company_a")
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.MessagingBlockManage,
		authz.Resource{Type: authz.TypeContactBlock, OrgID: &org, OwnerUserID: &student}), authz.ErrNotFound, false)

	// Without an org the company actions see nothing.
	wantErr(t, authz.Can(ctx, persona(t, "company_member"), authz.MessagingContactRequestCreate,
		authz.Resource{Type: authz.TypeContactRequest, Attrs: map[string]string{authz.AttrStatus: ""}}), authz.ErrNotFound, false)

	// Expiry is the system's only (03 §3.7); no HTTP action is a system one.
	wantErr(t, authz.Can(ctx, authz.SystemActor("messaging.expire_requests"), authz.MessagingContactRequestExpire,
		authz.Resource{}), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.MessagingContactRequestExpire,
		companyRequest("company_a", student)), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, authz.SystemActor("messaging.expire_requests"), authz.MessagingContactRequestRespond,
		ownRequest(student)), authz.ErrForbidden, false)
}

// conversation is the Resource of a conversation (BE-162): the student as
// owner; on the company path the company's org.
func conversation(org string, student uuid.UUID) authz.Resource {
	r := authz.Resource{Type: authz.TypeConversation, ID: uuid.NewV7(), OwnerUserID: &student,
		Attrs: map[string]string{authz.AttrStatus: "open"}}
	if org != "" {
		id := authztest.ID(org)
		r.OrgID = &id
	}
	return r
}

// TestMessagingPolicy_Conversations pins the conversation cells (BE-162):
// on the company path cmm S and cma S of the path's company (platform staff
// and the club's members 403, everyone else 404); on /me O for every
// signed-in user on their own conversation (another one's 404, anonymous
// 403); list_org only on the company path, list_own only on /me; cma needs
// MFA, the recruiter does not.
func TestMessagingPolicy_Conversations(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	student, other := authztest.ID("student"), authztest.ID("student_granted")
	acts := []authz.Action{authz.MessagingConversationRead, authz.MessagingConversationMarkRead, authz.MessagingConversationClose,
		authz.MessagingConversationMute, authz.MessagingMessageSend}
	staff := []string{"anonymous", "moderator", "support", "dpo", "platform_admin", "finance_admin", "talent_auditor"}
	for _, tc := range []struct {
		org   string
		allow []string
		see   []string
	}{
		{"company_a", []string{"company_member", "company_admin"}, staff},
		{"company_b", []string{"other_company_member"}, staff},
		{"club_a", nil, append([]string{"club_member", "club_admin"}, staff...)},
	} {
		for _, act := range append(acts, authz.MessagingConversationListOrg) {
			for _, p := range authztest.Personas() {
				var want error = notFound
				switch {
				case slices.Contains(tc.allow, p.Name):
					want = nil
				case slices.Contains(tc.see, p.Name):
					want = forbidden
				}
				wantErr(t, authz.Can(ctx, p.Actor, act, conversation(tc.org, student)), want, false)
			}
		}
	}
	for _, p := range authztest.Personas() {
		me := p.Actor.UserID
		for _, act := range append(acts, authz.MessagingConversationListOwn) {
			if p.Name == "anonymous" {
				wantErr(t, authz.Can(ctx, p.Actor, act, conversation("", other)), forbidden, false)
				continue
			}
			wantErr(t, authz.Can(ctx, p.Actor, act, conversation("", me)), nil, false)
			if me != other {
				wantErr(t, authz.Can(ctx, p.Actor, act, conversation("", other)), notFound, false)
			}
		}
	}
	// list_own is the /me list, list_org the company's: never the other path.
	wantErr(t, authz.Can(ctx, persona(t, "company_member"), authz.MessagingConversationListOwn, conversation("company_a", student)),
		notFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.MessagingConversationListOrg, conversation("", student)), notFound, false)
	// k-mfa on the admin's path only.
	cma, cmm := persona(t, "company_admin"), persona(t, "company_member")
	cma.MFAVerified, cmm.MFAVerified = false, false
	wantErr(t, authz.Can(ctx, cma, authz.MessagingMessageSend, conversation("company_a", student)), authz.ErrMFARequired, false)
	wantErr(t, authz.Can(ctx, cmm, authz.MessagingMessageSend, conversation("company_a", student)), nil, false)
	// The mandatory status attribute (03 §3.5).
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.MessagingConversationRead,
		authz.Resource{Type: authz.TypeConversation, OwnerUserID: &student}), forbidden, true)
}
