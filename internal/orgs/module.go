// Package orgs is the organisations module (01 §4.7; BE-017, BE-019): the
// single tenant table organizations (platform, university, club, company),
// the company profile, company registration (the founder becomes
// company_owner), profile updates with optimistic concurrency (ETag /
// If-Match), closing, ownership transfer and the platform admin's verify,
// suspend and reactivate; and the membership lifecycle: joining a club
// (open → active, approval → pending), approving and rejecting requests,
// invitations by e-mail, removing members, role changes, leaving, and the
// automatic end of club_admin when the student verification lapses or the
// student graduates (events plus the nightly sweep). The memberships are
// the Actor's org roles (hooks.memberships).
//
// Rules the package enforces:
//
//   - Org isolation (03 §3.6, §7.6): the management operations of an org
//     (/orgs/{org_id}/...) are invisible (404) to anyone who is not an
//     active member of that org or platform staff; a member without the
//     role gets 403. The public profile of a pending_review or closed org
//     is visible only to its members and to platform_admin, support and
//     moderator.
//   - Scope columns come from the parent row (06 H-38): a company profile,
//     a membership and an invitation take org_id and org_kind from their
//     organizations row, never from the request; a membership or an
//     invitation ID of another org is 404.
//   - A suspended org reads normally, but writes on its behalf are 403
//     orgs.org_suspended (03 §2.3); a platform admin may still act on it.
//   - R operations (closeOrg, transferOrgOwnership, adminSuspendOrg,
//     adminReactivateOrg) verify the re-authentication proof after
//     authz.Can and before any business data (03 §6.2).
//   - An org never loses its last club_admin or its company_owner through
//     the HTTP operations (03 k6: 409 orgs.last_admin); only the system
//     paths (lapsed verification, graduation, closing, erasure) may.
//   - club_admin goes to verified adult students only (03 k5, T-55).
//   - Sensitive clubs (02 §7.3): members do not see each other (T-20), the
//     member list is the club_admin's only and every read of it is
//     audited; platform staff see nothing (Y2, 404); an ended membership is
//     deleted at once (T-83).
//   - Invitations: an opaque token derived from the invitation ID (HMAC,
//     crypto.Deriver), single use, 7 days; only its SHA-256 is stored and
//     the token never enters an event or a log. The accepting account's
//     e-mail must equal the invited address (otherwise 404).
//   - Personal data: organizations and companies hold none (02 §7.1–7.2;
//     tax number and MERSİS live in parties). Memberships and invitations
//     are personal data; logs carry IDs only, never names or addresses.
package orgs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
)

// OrgDirectory is the read contract of other modules (01 §4.7).
type OrgDirectory interface {
	// Get returns the summary of an org (any status) or ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (OrgSummary, error)
	// GetMany returns the summaries of the ids that exist.
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]OrgSummary, error)
	// MembershipsOf returns the active memberships of a user in every org,
	// sensitive clubs included (Actor resolution, 03 §2.1; the result is
	// never serialised to anyone but the user).
	MembershipsOf(ctx context.Context, userID uuid.UUID) ([]authz.Membership, error)
	// OpenMembershipsOf returns the status (pending or active) of every
	// open membership of a user by org, sensitive clubs included (the
	// reader's own relations, BE-122; never shown to anyone but the user).
	OpenMembershipsOf(ctx context.Context, userID uuid.UUID) (map[uuid.UUID]MemberStatus, error)
	// HasRole reports whether userID holds one of roles in orgID (active).
	HasRole(ctx context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error)
	// ActiveMemberIDs returns the user IDs of an org's active members (a
	// club notifying its own members).
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
}

// MembershipHistory is the membership history read of other modules
// (BE-090): talent's reconciliation of a closed period (00 T-321) asks
// which memberships its ledger credited have ended. A system read without
// authorisation; only the IDs of the ended rows leave.
type MembershipHistory interface {
	// EndedMemberships returns the memberships among membershipIDs that
	// ended (status ended: what membership.ended reports). An unknown ID
	// (never existed, or deleted: a sensitive club's ended membership, the
	// retention purge) is missing.
	EndedMemberships(ctx context.Context, membershipIDs []uuid.UUID) (map[uuid.UUID]bool, error)
}

// OrgLogos is the public logo read of other modules (BE-126): the
// cards that list orgs elsewhere (directory's club list) show the same
// permanent logo URL as the org's own profile, under the one rule of
// BE-120 kept here.
type OrgLogos interface {
	// PublicLogoURLs returns the permanent public logo URL of each of ids
	// that shows one: an active org that is not a sensitive club, once
	// files cleared and copied the image. The others are missing; a
	// missing logo never fails the read.
	PublicLogoURLs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// OrgAdmin is the write contract of other modules (01 §4.7).
type OrgAdmin interface {
	// Create writes a new org (and the company profile of a company) in
	// the transaction carried by ctx, joining the caller's: a rollback of
	// the caller removes the org too. The caller audits its own action.
	Create(ctx context.Context, in CreateOrg) (uuid.UUID, error)
	// GrantRole gives userID role in orgID in the caller's transaction
	// (directory's claim approval, 01 §6.3): an open membership of the
	// user ends (role_changed) and an active one with the role starts
	// (source claim). Granting the role the user already holds is a no-op.
	// club_admin needs a verified adult student (422
	// orgs.club_admin_requires_adult_student); company_owner is never
	// granted here. Like Create, the caller audits its own decision (with
	// reason: org_members has no column for it).
	GrantRole(ctx context.Context, orgID, userID uuid.UUID, role authz.MemberRole, reason string) error
	// EndRoles ends the active memberships of userID holding one of roles
	// in every org (system action orgs.member.end_role: lapsed
	// verification, graduation, claim revocation) and returns how many
	// ended. It bypasses the last-admin rule: the reason is mandatory.
	EndRoles(ctx context.Context, userID uuid.UUID, roles []authz.MemberRole, reason EndReason) (int, error)
}

// Service is the orgs service: the cross-module contracts plus the HTTP use
// cases (04 §2.2). Every method that takes an actor calls authz.Can first
// (05 §5.3).
type Service interface {
	OrgDirectory
	OrgAdmin
	MembershipHistory
	OrgLogos

	// GetOrg is getOrg (orgs.org.read, P).
	GetOrg(ctx context.Context, a authz.Actor, id uuid.UUID) (OrgView, error)
	// GetOrgBySlug is getOrgBySlug (orgs.org.read, P).
	GetOrgBySlug(ctx context.Context, a authz.Actor, slug string) (OrgView, error)
	// SearchOrgs is searchOrgs (orgs.org.search, P; rate class search).
	SearchOrgs(ctx context.Context, a authz.Actor, q SearchQuery) (OrgPage, error)
	// ListCompanies is listCompanies (orgs.org.search, P; BE-120): the
	// public company list, active companies only.
	ListCompanies(ctx context.Context, a authz.Actor, q CompanyListQuery) (OrgPage, error)
	// CreateCompany is createCompany (orgs.company.create): the company
	// starts pending_review, the caller becomes its company_owner and the
	// caller's sessions are closed with role_change after the commit.
	CreateCompany(ctx context.Context, a authz.Actor, in CompanyInput) (OrgView, error)
	// UpdateOrg is updateOrg (orgs.org.update): ifMatch is the If-Match
	// header (04 §1.13: missing → 428, stale → 412).
	UpdateOrg(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p OrgPatch) (OrgView, error)
	// CloseOrg is closeOrg (orgs.org.close, R): 409 orgs.open_commerce
	// while an open-commerce check reports open orders or payouts.
	CloseOrg(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof identity.ReauthProof) (OrgView, error)
	// TransferOwnership is transferOrgOwnership (orgs.ownership.transfer, R):
	// the new owner must be an active company_admin of the company.
	TransferOwnership(ctx context.Context, a authz.Actor, id, newOwner uuid.UUID, proof identity.ReauthProof) (OrgView, error)
	// AdminListOrgs is adminListOrgs (orgs.org.list_admin).
	AdminListOrgs(ctx context.Context, a authz.Actor, q AdminQuery) (OrgPage, error)
	// AdminVerifyOrg is adminVerifyOrg (orgs.org.verify): pending_review →
	// active with verified_at.
	AdminVerifyOrg(ctx context.Context, a authz.Actor, id uuid.UUID, note string) (OrgView, error)
	// AdminSuspendOrg is adminSuspendOrg (orgs.org.suspend, R): active →
	// suspended.
	AdminSuspendOrg(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof identity.ReauthProof) (OrgView, error)
	// AdminReactivateOrg is adminReactivateOrg (orgs.org.suspend, R):
	// suspended → active.
	AdminReactivateOrg(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof identity.ReauthProof) (OrgView, error)

	// ListMembers is listOrgMembers (orgs.member.list; B for support and
	// platform_admin). A sensitive club's list is its club_admin's only and
	// every read is audited.
	ListMembers(ctx context.Context, a authz.Actor, orgID uuid.UUID, q MemberQuery, access Access) (MemberPage, error)
	// GetMember is getOrgMember (orgs.member.read): an open membership.
	GetMember(ctx context.Context, a authz.Actor, orgID, id uuid.UUID, access Access) (MemberView, error)
	// ApproveJoinRequest is approveJoinRequest (orgs.member.decide):
	// pending → active, membership.started.
	ApproveJoinRequest(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (MemberView, error)
	// RejectJoinRequest is rejectJoinRequest (orgs.member.decide): pending
	// → ended(rejected); the reason is never shown to the student.
	RejectJoinRequest(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (MemberView, error)
	// RemoveMember is removeOrgMember (orgs.member.remove): active →
	// ended(removed); 409 orgs.last_admin for the last admin.
	RemoveMember(ctx context.Context, a authz.Actor, orgID, id uuid.UUID, reason string) error
	// ChangeMemberRole is changeOrgMemberRole (orgs.member.change_role):
	// the old row ends (role_changed) and a new active row starts.
	ChangeMemberRole(ctx context.Context, a authz.Actor, orgID, id uuid.UUID, role authz.MemberRole) (MemberView, error)
	// ListMyMemberships is listMyMemberships (orgs.membership.list_own).
	ListMyMemberships(ctx context.Context, a authz.Actor, q MyMembershipQuery) (MyMembershipPage, error)
	// LeaveMembership is leaveMyMembership (orgs.membership.leave): the
	// caller's pending or active membership ends (left).
	LeaveMembership(ctx context.Context, a authz.Actor, id uuid.UUID) error
	// CreateInvitation is createOrgInvitation (orgs.invitation.create).
	CreateInvitation(ctx context.Context, a authz.Actor, orgID uuid.UUID, email string, role authz.MemberRole) (Invitation, error)
	// ListInvitations is listOrgInvitations (orgs.invitation.list): open,
	// unexpired invitations.
	ListInvitations(ctx context.Context, a authz.Actor, orgID uuid.UUID, q InvitationQuery) (InvitationPage, error)
	// RevokeInvitation is revokeOrgInvitation (orgs.invitation.revoke).
	RevokeInvitation(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) error
	// AcceptInvitation is acceptOrgInvitation (orgs.invitation.accept): the
	// token's invitation, addressed to the caller's own e-mail (otherwise
	// 404), becomes an active membership.
	AcceptInvitation(ctx context.Context, a authz.Actor, token string) (Membership, error)
	// JoinClub is joinClub (orgs.member.join): open policy → active,
	// approval → pending (T-77: the club facts come from ClubInfo).
	JoinClub(ctx context.Context, a authz.Actor, clubID uuid.UUID, message string) (Membership, error)
}

// Deps are the dependencies of the orgs module. Store, Tx, Outbox, Audit,
// Idempotency, Clock and Log are required (New returns an error otherwise).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Outbox      outbox.Writer       // org_invitation.created (O); membership.started / membership.ended (A)
	Audit       AuditLogger         // every write, same transaction
	Idempotency *idempotency.Keeper // optional Idempotency-Key of the POST operations (06 H-5)
	Clock       clock.Clock
	Log         *slog.Logger

	// Reauth verifies the proof of the R operations (identity). nil →
	// they fail closed with 503 (problem.ErrUnavailable).
	Reauth Reauthenticator
	// Files attaches, serves and (file.rejected, T-176) deletes the org
	// logo. nil → logo_file_id is refused (503), no logo_url /
	// logo_public_url is returned and orgs.drop_rejected_logo fails (retried).
	Files Files
	// RevokeSessions closes a user's sessions after a role change
	// (role_change). nil → nothing is revoked (logged).
	RevokeSessions SessionRevoker
	// OpenCommerce returns the open-commerce checks of closeOrg at call
	// time (hooks.openCommerce is filled by later layers). nil or empty →
	// no check.
	OpenCommerce func() []OpenCommerceCheck
	// UniversityOrgs resolves searchOrgs?university_id. nil → the filter
	// matches nothing.
	UniversityOrgs UniversityOrgs

	// Users is identity's user directory (member names, the e-mail match of
	// acceptOrgInvitation, the age of a club_admin candidate). nil → names
	// are empty and acceptOrgInvitation / club_admin grants fail closed.
	Users Users
	// Students is identity's student verification. nil → nobody is a
	// verified student (club_admin is refused) and the admin sweep does
	// nothing.
	Students Students
	// Sender delivers the invitation e-mail and the review notices
	// (worker). nil → those tasks retry.
	Sender Sender
	// PlatformAdmins lists the recipients of a new org's review request
	// (BE-144). nil → that task retries.
	PlatformAdmins PlatformAdmins
	// Clubs returns a club's join facts (hooks.clubInfo, directory). nil →
	// joinClub 404 and every club counts as sensitive (no logo_public_url
	// for any club, BE-120).
	Clubs ClubInfo
	// Graduated reports graduated students for the admin sweep (students,
	// late bound). nil → only the verification is checked.
	Graduated GraduatedUsers
	// Avatars returns the member avatars of the member list (students,
	// late bound; BE-023). nil → no avatar_url.
	Avatars AvatarURLs
	// Deriver derives invitation tokens (CRYPTO_TOKEN_KEYS). nil →
	// createOrgInvitation answers 503 and acceptOrgInvitation finds nothing.
	Deriver *crypto.Deriver
	// LinkBaseURL is the web application origin of the invitation link
	// (APP_LINK_BASE_URL; internal/platform/applink).
	LinkBaseURL string
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Outbox", d.Outbox != nil}, {"Audit", d.Audit != nil},
		{"Idempotency", d.Idempotency != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("orgs.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what orgs hands to the composition root.
type Module struct {
	Service Service
	// Privacy is the KVKK provider (memberships, invitations and registered
	// orgs of a user; the org rows themselves hold no personal data).
	Privacy privacy.Provider
	// Sweeper ends the retention of ended memberships and closed
	// invitations (02 §24.2; BE-057).
	Sweeper privacy.RetentionSweeper

	svc *service
}

// New builds the orgs module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	w := sweeper{retention: d.Store, tx: d.Tx}
	return Module{Service: s, Privacy: privacyProvider{s: s}, Sweeper: w, svc: s}, nil
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
