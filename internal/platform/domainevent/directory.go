package domainevent

import "uuid"

// Topics of the directory module (01 §6.2). Owner: directory tickets
// (BE-020 adds club.sensitivity_changed here).
const (
	TopicClubClaimDecided = "club_claim.decided"
)

var _ = register(
	spec("directory", Outbox, ClubClaimDecided{}),
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
