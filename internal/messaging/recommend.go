package messaging

import (
	"context"
	"time"
	"uuid"

	"koro/internal/feed"
)

// ContactedOpportunities is the contact request read of the rule-based
// recommendations (BE-198; recommend, BE-191): the opportunity calls a
// company already contacted the student about (createContactRequest with
// opportunity_post_id, 04 §2.9) are not recommended to the student again.
// A system read of one student's own requests, without authorisation: the
// caller (recommend, L13: messaging never imports it) authorises its own
// read first (cell O: the student's own data for the student only). Call
// IDs only, in one query; nothing about the company, the status or the
// message leaves.
type ContactedOpportunities interface {
	// ContactedOpportunityPostIDs returns up to limit (at most
	// feed.MaxExcludedOpportunities) opportunity calls named by the
	// requests addressed to userID created at or after since, whatever
	// their status (pending or accepted: the student is in touch; declined:
	// the student said no; expired or withdrawn: the student saw it),
	// latest request first.
	ContactedOpportunityPostIDs(ctx context.Context, userID uuid.UUID, since time.Time, limit int) ([]uuid.UUID, error)
}

var _ ContactedOpportunities = (*service)(nil)

// ContactedOpportunityPostIDs implements ContactedOpportunities.
func (s *service) ContactedOpportunityPostIDs(ctx context.Context, userID uuid.UUID, since time.Time, limit int) ([]uuid.UUID, error) {
	limit = min(limit, feed.MaxExcludedOpportunities)
	if limit <= 0 || userID == uuid.Nil() {
		return []uuid.UUID{}, nil
	}
	return s.requests().OpportunityPostIDsOf(ctx, userID, since, limit)
}
