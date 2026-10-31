package feed

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/authz"
)

// anonymousReads reports whether an anonymous reader opens p (03 §3.6,
// T-57): the post is on the public web. The rule is authz's, not a copy;
// public and followers posts carry ['all'], so the audience match holds.
func anonymousReads(ctx context.Context, p Post) bool {
	return authz.Can(ctx, authz.Anonymous(), authz.FeedPostRead, postResource(p, true, false)) == nil
}

// PublicPost implements PublicPosts (webcache, BE-150): a system read
// without an actor.
func (s *service) PublicPost(ctx context.Context, postID uuid.UUID) (PostSummary, bool, error) {
	p, err := s.posts().GetPost(ctx, postID)
	switch {
	case errors.Is(err, ErrNotFound):
		return PostSummary{}, false, nil
	case err != nil:
		return PostSummary{}, false, err
	case !anonymousReads(ctx, p):
		return PostSummary{}, false, nil
	}
	return summaryOf(p), true, nil
}

// PublicEventShares implements PublicPosts (webcache: one event, BE-150;
// an organiser's events, BE-157): the anonymous reader's filter of the org
// pages (readerOf, readerConds).
func (s *service) PublicEventShares(ctx context.Context, eventIDs []uuid.UUID, limit int) ([]PostSummary, error) {
	out := []PostSummary{}
	if limit <= 0 || len(eventIDs) == 0 {
		return out, nil
	}
	r, err := s.readerOf(ctx, authz.Anonymous())
	if err != nil {
		return nil, err
	}
	rows, err := s.posts().ListEventShares(ctx, eventIDs, r, limit)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		out = append(out, summaryOf(p))
	}
	return out, nil
}
