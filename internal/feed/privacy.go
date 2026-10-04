package feed

import (
	"context"
	"uuid"

	"koro/internal/platform/privacy"
)

// privacyRetainedReason explains what Erase leaves in place.
const privacyRetainedReason = "posts are the author org's content (02 §12.2); author_user_id and reviewed_by " +
	"reference the account, which identity anonymises last (the tombstone keeps no name or address), so a post " +
	"no longer identifies its author; readers never see the author user. Reports are BE-032's"

// privacyProvider is the KVKK provider of feed (01 §3.3). What it knows
// about a person:
//
//   - the posts they wrote on behalf of an org (posts.author_user_id,
//     personal per 02 §12.2). Export lists them with their org, kind,
//     status and dates (the content is the org's). Erase keeps the rows:
//     the post belongs to the org and the reference is anonymised by
//     identity's tombstone (author_user_id is NOT NULL and points at
//     users); the count is reported as retained with the reason.
//   - their likes, saves and hides of posts and their follows and mutes
//     of orgs (02 §12.4–12.8, personal). Export lists them; Erase deletes
//     them (hard delete) and takes the likes and saves off the posts'
//     counters.
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
	in, err := p.s.d.Store.InteractionsOf(ctx, userID)
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
	for _, x := range []struct {
		typ  string
		rows []Interaction
	}{{"post_like", in.Likes}, {"post_save", in.Saves}, {"post_hide", in.Hides}} {
		for _, r := range x.rows {
			out.Items = append(out.Items, map[string]any{"type": x.typ, "post_id": r.PostID.String(), "created_at": r.CreatedAt})
		}
	}
	for _, f := range in.Follows {
		out.Items = append(out.Items, map[string]any{
			"type": "org_follow", "org_id": f.OrgID.String(), "source": string(f.Source), "notify": f.Notify,
			"created_at": f.CreatedAt,
		})
	}
	for _, m := range in.Mutes {
		out.Items = append(out.Items, map[string]any{"type": "org_mute", "org_id": m.OrgID.String(), "created_at": m.CreatedAt})
	}
	return out, nil
}

// Erase implements privacy.Provider; idempotent (a second run finds no
// interaction and changes no post).
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	res := privacy.EraseResult{Module: "feed"}
	err := p.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		n, err := p.s.d.Store.EraseInteractions(ctx, userID)
		if err != nil {
			return err
		}
		rows, err := p.s.d.Store.PostsOfAuthor(ctx, userID)
		if err != nil {
			return err
		}
		res.Deleted, res.Retained = n, len(rows)
		return nil
	})
	if err != nil {
		return privacy.EraseResult{}, err
	}
	if res.Retained > 0 {
		res.RetainedReason = privacyRetainedReason
	}
	return res, nil
}
