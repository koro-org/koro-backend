package students

import (
	"context"
	"uuid"

	"koro/internal/platform/db"
)

// PopularClubs implements recommendStore: one grouping pass over the
// eligible profiles that hold dim (student_profiles_audience_idx, a GIN
// index on audience_keys of the eligible rows).
//
// cross-org: keyed by a department or university key (student profiles
// belong to no org); only club IDs leave, the counts stay in the query.
func (s gormStore) PopularClubs(ctx context.Context, dim string, min, limit int) ([]uuid.UUID, error) {
	var rows []struct {
		OrgID uuid.UUID `gorm:"type:uuid;column:org_id"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT substr(k.key, 6)::uuid AS org_id
		  FROM student_profiles sp CROSS JOIN LATERAL unnest(sp.audience_keys) AS k(key)
		 WHERE sp.audience_eligible
		   AND sp.audience_keys @> CAST( @dims AS text[])
		   AND k.key LIKE 'club:%'
		 GROUP BY k.key
		HAVING count(*) >= @min
		 ORDER BY count(*) DESC, k.key
		 LIMIT @limit`, map[string]any{"dims": db.TextArray([]string{dim}), "min": min, "limit": limit}).Scan(&rows).Error
	if err != nil {
		return nil, wrap("popular clubs", err)
	}
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.OrgID
	}
	return out, nil
}
