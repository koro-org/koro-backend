package domainevent

import "uuid"

// Topics of the orgs module (01 §6.2). Owner: orgs tickets.
const (
	TopicOrgInvitationCreated = "org_invitation.created"
	TopicMembershipStarted    = "membership.started"
	TopicMembershipEnded      = "membership.ended"
)

var _ = register(
	spec("orgs", Outbox, OrgInvitationCreated{}),
	spec("orgs", AfterCommit, MembershipStarted{}),
	spec("orgs", AfterCommit, MembershipEnded{}),
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
