package domainevent

import "uuid"

// Topics of the orgs module (01 §6.2). Owner: orgs tickets.
const (
	TopicOrgInvitationCreated = "org_invitation.created"
	TopicMembershipStarted    = "membership.started"
	TopicMembershipEnded      = "membership.ended"
	TopicOrgUpdated           = "org.updated"
	TopicOrgReviewRequested   = "org.review_requested"
	TopicOrgReviewDecided     = "org.review_decided"
)

var _ = register(
	spec("orgs", Outbox, OrgInvitationCreated{}),
	spec("orgs", AfterCommit, MembershipStarted{}),
	spec("orgs", AfterCommit, MembershipEnded{}),
	spec("orgs", Outbox, OrgUpdated{}),
	spec("orgs", Outbox, OrgReviewRequested{}),
	spec("orgs", Outbox, OrgReviewDecided{}),
)

// OrgInvitationCreated: the worker sends the invitation e-mail.
type OrgInvitationCreated struct {
	InvitationID uuid.UUID `json:"invitation_id"`
	OrgID        uuid.UUID `json:"org_id"`
}

// Topic implements Payload.
func (OrgInvitationCreated) Topic() string { return TopicOrgInvitationCreated }

// MembershipStarted: a user became a member of an org.
type MembershipStarted struct {
	MembershipID uuid.UUID `json:"membership_id"`
	OrgID        uuid.UUID `json:"org_id"`
	OrgKind      string    `json:"org_kind"`
	UserID       uuid.UUID `json:"user_id"`
	Role         string    `json:"role"`
}

// Topic implements Payload.
func (MembershipStarted) Topic() string { return TopicMembershipStarted }

// MembershipEnded: a membership ended (left, removed, role dropped).
type MembershipEnded struct {
	MembershipID uuid.UUID `json:"membership_id"`
	OrgID        uuid.UUID `json:"org_id"`
	OrgKind      string    `json:"org_kind"`
	UserID       uuid.UUID `json:"user_id"`
	Role         string    `json:"role"`
	Reason       string    `json:"reason"`
}

// Topic implements Payload.
func (MembershipEnded) Topic() string { return TopicMembershipEnded }

// OrgUpdated: the public profile of an org changed (BE-106): updateOrg
// (Fields = the changed field names) or a status change (verify, suspend,
// reactivate, close: Fields = ["status"]). The web revalidates the org's
// public page.
type OrgUpdated struct {
	OrgID   uuid.UUID `json:"org_id"`
	OrgKind string    `json:"org_kind"`
	Fields  []string  `json:"fields"`
}

// Topic implements Payload.
func (OrgUpdated) Topic() string { return TopicOrgUpdated }

// OrgReviewRequested: an org that awaits the platform admin's verification
// was registered (createCompany: pending_review; BE-144). The worker tells
// the platform admins in-app.
type OrgReviewRequested struct {
	OrgID   uuid.UUID `json:"org_id"`
	OrgKind string    `json:"org_kind"`
}

// Topic implements Payload.
func (OrgReviewRequested) Topic() string { return TopicOrgReviewRequested }

// Outcomes of OrgReviewDecided.
const (
	OrgReviewVerified = "verified" // adminVerifyOrg: pending_review → active
	OrgReviewRejected = "rejected" // closeOrg of a pending_review org by platform staff
)

// OrgReviewDecided: the review of a pending_review org ended (BE-144). The
// worker tells the org's admins of a verification (in-app + e-mail) and the
// founder of a rejection (in-app + e-mail with the reason, which the event
// never carries: the worker reads organizations.rejection_reason).
type OrgReviewDecided struct {
	OrgID   uuid.UUID `json:"org_id"`
	OrgKind string    `json:"org_kind"`
	Outcome string    `json:"outcome"`
}

// Topic implements Payload.
func (OrgReviewDecided) Topic() string { return TopicOrgReviewDecided }
