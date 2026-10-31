package directory

import (
	"context"
	"uuid"

	"koro/internal/platform/db"
)

// UniversityByOrg implements StatsStore.
//
// cross-org: the public reference catalog, looked up by the university's org.
func (s gormStore) UniversityByOrg(ctx context.Context, orgID uuid.UUID) (University, error) {
	return take[University]("university by org", uniCols, db.Conn(ctx, s.orm).Where("org_id = ?", orgID))
}

// ClubsOf implements StatsStore.
//
// cross-org: the public club catalog of one university.
func (s gormStore) ClubsOf(ctx context.Context, universityID uuid.UUID) ([]Club, error) {
	var out []Club
	err := db.Conn(ctx, s.orm).Select(clubCols).Where("university_id = ?", universityID).Order("org_id").Find(&out).Error
	if err != nil {
		return nil, wrap("clubs of", err)
	}
	return out, nil
}
