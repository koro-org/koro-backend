package feed

import (
	"context"
	"strings"

	"koro/internal/platform/db"
)

// RecommendableOpportunities implements Store over posts_feed_idx.
//
// cross-org: the recommendation candidates span every author org (BE-191);
// the reader's visibility (03 §3.6) is applied in the query, as in
// ListFeed.
func (s gormStore) RecommendableOpportunities(ctx context.Context, f OpportunityFilter, limit int) ([]Post, error) {
	var match []string
	var args []any
	if len(f.OrgIDs) > 0 {
		match, args = append(match, "p.org_id IN ?"), append(args, f.OrgIDs)
	}
	if f.ClassYear > 0 {
		match, args = append(match, "p.opportunity -> 'eligible_years' @> jsonb_build_array( ?::int)"), append(args, f.ClassYear)
	}
	if f.MinInterest > 0 {
		match, args = append(match, "p.like_count >= ? OR p.save_count >= ?"), append(args, f.MinInterest, f.MinInterest)
	}
	if len(match) == 0 || limit <= 0 || f.Reader.UserID == nil {
		return []Post{}, nil
	}
	u := *f.Reader.UserID
	q := db.Conn(ctx, s.orm).Table("posts AS p").Select(prefixed).
		Where("p.status = 'published' AND p.kind = ? AND p.published_at >= ?", string(KindOpportunityCall), f.Since).
		// The deadline is a validated YYYY-MM-DD (input.go): byte order is date order.
		Where(`(p.opportunity ->> 'deadline' IS NULL OR (p.opportunity ->> 'deadline') COLLATE "C" >= ?)`, f.Today.String())
	cond, cargs := readerConds(f.Reader)
	q = q.Where(cond, cargs...).
		Where("NOT EXISTS (SELECT 1 FROM org_mutes m WHERE m.user_id = ? AND m.org_id = p.org_id)", u).
		Where("NOT EXISTS (SELECT 1 FROM post_hides h WHERE h.user_id = ? AND h.post_id = p.id)", u).
		Where("NOT EXISTS (SELECT 1 FROM post_reactions r WHERE r.user_id = ? AND r.post_id = p.id)", u).
		Where("NOT EXISTS (SELECT 1 FROM post_saves v WHERE v.user_id = ? AND v.post_id = p.id)", u).
		Where("("+strings.Join(match, " OR ")+")", args...)
	if len(f.ExcludeIDs) > 0 {
		q = q.Where("p.id NOT IN ?", f.ExcludeIDs)
	}
	var out []Post
	if err := q.Order("p.published_at DESC, p.id DESC").Limit(limit).Scan(&out).Error; err != nil {
		return nil, wrap("recommendable opportunities", err)
	}
	return out, nil
}
