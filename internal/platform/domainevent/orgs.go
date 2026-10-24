package domainevent

import "uuid"

// Topics of the orgs module (01 §6.2). Owner: orgs tickets.
const (
	TopicOrgInvitationCreated = "org_invitation.created"
	TopicMembershipStarted    = "membership.started"
	TopicMembershipEnded      = "membership.ended"
	TopicOrgUpdated           = "org.updated"
)

var _ = register(
	spec("orgs", Outbox, OrgInvitationCreated{}),
	spec("orgs", AfterCommit, MembershipStarted{}),
	spec("orgs", AfterCommit, MembershipEnded{}),
	spec("orgs", Outbox, OrgUpdated{}),
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
