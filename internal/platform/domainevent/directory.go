package domainevent

import "uuid"

// Topics of the directory module (01 §6.2). Owner: directory tickets
// (BE-020 added club.sensitivity_changed, T-80; BE-106 club.updated).
const (
	TopicClubClaimDecided       = "club_claim.decided"
	TopicClubSensitivityChanged = "club.sensitivity_changed"
	TopicClubUpdated            = "club.updated"
)

var _ = register(
	spec("directory", Outbox, ClubClaimDecided{}),
	spec("directory", Outbox, ClubSensitivityChanged{}),
	spec("directory", Outbox, ClubUpdated{}),
)

// ClubClaimDecided: a club claim was approved or rejected.
type ClubClaimDecided struct {
	ClaimID   uuid.UUID `json:"claim_id"`
	ClubOrgID uuid.UUID `json:"club_org_id"`
	UserID    uuid.UUID `json:"user_id"`
	Outcome   string    `json:"outcome"`
}

// Topic implements Payload.
func (ClubClaimDecided) Topic() string { return TopicClubClaimDecided }

// ClubSensitivityChanged: a platform admin set or cleared a club's
// sensitive flag (02 §8.5, T-80). Written in the same transaction as the
// flag; students (BE-023) drops the club's audience keys from every profile
// when Sensitive is true (students.audience_refresh is the nightly safety
// net). The reason code is not carried: consumers need the flag only.
type ClubSensitivityChanged struct {
	ClubOrgID uuid.UUID `json:"club_org_id"`
	Sensitive bool      `json:"sensitive"`
}

// Topic implements Payload.
func (ClubSensitivityChanged) Topic() string { return TopicClubSensitivityChanged }

// ClubUpdated: the club profile changed (BE-106): updateClub (Fields = the
// changed field names) or adminCreateClub (Fields = ["created"]). The web
// revalidates the club's public page.
type ClubUpdated struct {
	ClubOrgID uuid.UUID `json:"club_org_id"`
	Fields    []string  `json:"fields"`
}

// Topic implements Payload.
func (ClubUpdated) Topic() string { return TopicClubUpdated }
