package feed

import (
	"context"
	"uuid"

	"koro/internal/platform/privacy"
)

// privacyRetainedReason explains what Erase leaves in place.
const privacyRetainedReason = "posts are the author org's content (02 §12.2); author_user_id and reviewed_by " +
	"reference the account, which identity anonymises last (the tombstone keeps no name or address), so a post " +
	"no longer identifies its author; readers never see the author user. Likes, saves, hides, follows and mutes " +
	"are BE-029's, reports BE-032's"

// privacyProvider is the KVKK provider of the feed core (01 §3.3). What it
// knows about a person is the posts they wrote on behalf of an org
// (posts.author_user_id, personal per 02 §12.2). Export lists them with
// their org, kind, status and dates (the content is the org's). Erase keeps
// the rows: the post belongs to the org and the reference is anonymised by
// identity's tombstone (author_user_id is NOT NULL and points at users);
// the count is reported as retained with the reason.
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "feed" }

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	rows, err := p.s.d.Store.PostsOfAuthor(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out := privacy.ExportSection{Module: "feed", Items: make([]map[string]any, 0, len(rows))}
	for _, r := range rows {
		item := map[string]any{
			"type": "post", "post_id": r.ID.String(), "org_id": r.OrgID.String(), "kind": string(r.Kind),
			"status": string(r.Status), "created_at": r.CreatedAt, "published_at": r.PublishedAt,
		}
		if r.Title != nil {
			item["title"] = *r.Title
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Erase implements privacy.Provider; idempotent (it changes nothing).
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	rows, err := p.s.d.Store.PostsOfAuthor(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	res := privacy.EraseResult{Module: "feed", Retained: len(rows)}
	if len(rows) > 0 {
		res.RetainedReason = privacyRetainedReason
	}
	return res, nil
}
