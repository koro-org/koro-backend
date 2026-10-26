package directory

import (
	"context"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/db"
)

// claimCols are the columns of club_claims (02 §8.6).
const claimCols = `id, club_org_id, claimant_user_id, status, role_in_club, evidence_text, evidence_file_id,
	reviewed_by, reviewed_at, decision_note, created_at, updated_at`

func claimArgs(c ClubClaim) map[string]any {
	return map[string]any{
		"id": c.ID, "club": c.ClubOrgID, "claimant": c.ClaimantUserID, "status": string(c.Status), "role": c.RoleInClub,
		"text": c.EvidenceText, "file": c.EvidenceFileID, "reviewed_by": c.ReviewedBy, "reviewed_at": c.ReviewedAt,
		"note": c.DecisionNote,
	}
}

// InsertClaim implements Store.
//
// cross-org: club_org_id is the claimed club, read by the service from the
// clubs row (06 H-38), never from the request alone.
func (s gormStore) InsertClaim(ctx context.Context, c ClubClaim) (ClubClaim, error) {
	return returning[ClubClaim](ctx, s.orm, "insert claim", `INSERT INTO club_claims
		(id, club_org_id, claimant_user_id, status, role_in_club, evidence_text, evidence_file_id)
		SELECT @id, c.org_id, @claimant, @status, @role, @text, @file FROM clubs c WHERE c.org_id = @club
		RETURNING `+claimCols, claimArgs(c))
}

// GetClaim implements Store.
//
// cross-org: a claim belongs to its claimant; the service decides with
// authz after the read (owner or platform staff).
func (s gormStore) GetClaim(ctx context.Context, id uuid.UUID) (ClubClaim, error) {
	return take[ClubClaim]("get claim", claimCols, db.Conn(ctx, s.orm).Where("id = ?", id))
}

// LockClaim implements Store.
//
// cross-org: as GetClaim.
func (s gormStore) LockClaim(ctx context.Context, id uuid.UUID) (ClubClaim, error) {
	return take[ClubClaim]("lock claim", claimCols, locked(db.Conn(ctx, s.orm)).Where("id = ?", id))
}

// UpdateClaim implements Store.
//
// cross-org: as GetClaim (the club_org_id of the row never changes).
func (s gormStore) UpdateClaim(ctx context.Context, c ClubClaim) (ClubClaim, error) {
	return returning[ClubClaim](ctx, s.orm, "update claim", `UPDATE club_claims SET
		status = @status, evidence_file_id = @file, reviewed_by = @reviewed_by, reviewed_at = @reviewed_at,
		decision_note = @note
		WHERE id = @id RETURNING `+claimCols, claimArgs(c))
}

// ClaimsOf implements Store.
//
// cross-org: the claims of one claimant (listMyClubClaims, KVKK export).
func (s gormStore) ClaimsOf(ctx context.Context, userID uuid.UUID, after *uuid.UUID, limit int) ([]ClubClaim, error) {
	q := db.Conn(ctx, s.orm).Model(&ClubClaim{}).Select(claimCols).Where("claimant_user_id = ?", userID)
	if after != nil {
		q = q.Where("id < ?", *after)
	}
	return claimRows("claims of", q.Order("id DESC").Limit(limit))
}

// ListClaims implements Store.
//
// cross-org: the platform staff's review queue spans every club; a
// university_rep's is scoped to their universities' non-sensitive clubs
// (directory's own clubs and universities rows).
func (s gormStore) ListClaims(ctx context.Context, f ClaimFilter, after *uuid.UUID, limit int) ([]ClubClaim, error) {
	status := f.Status
	q := db.Conn(ctx, s.orm).Model(&ClubClaim{}).Select(claimCols).Where("status = ?", string(status))
	if f.RepUniversityOrgs != nil {
		q = q.Where(`club_org_id IN (SELECT c.org_id FROM clubs c JOIN universities u ON u.id = c.university_id
			WHERE NOT c.is_sensitive AND u.org_id = ANY( ?))`, db.UUIDArray(f.RepUniversityOrgs))
	}
	if f.UniversityOrg != nil {
		q = q.Where(`club_org_id IN (SELECT c.org_id FROM clubs c JOIN universities u ON u.id = c.university_id
			WHERE u.org_id = ?)`, *f.UniversityOrg)
	}
	if f.RepUniversityOrgs != nil && f.RepPendingSince != nil && status == ClaimPending {
		q = q.Where("created_at > ?", *f.RepPendingSince)
	}
	if f.CreatedBefore != nil {
		q = q.Where("created_at <= ?", *f.CreatedBefore)
	}
	switch {
	case status == ClaimPending && after != nil:
		q = q.Where("id > ?", *after)
	case after != nil:
		q = q.Where("id < ?", *after)
	}
	order := "id DESC"
	if status == ClaimPending {
		order = "id" // oldest first: club_claims_queue_idx orders by created_at, UUIDv7 ids follow it
	}
	return claimRows("list claims", q.Order(order).Limit(limit))
}

// ClaimOfFile implements Store.
//
// cross-org: file.rejected names the file, not the club.
func (s gormStore) ClaimOfFile(ctx context.Context, fileID uuid.UUID) (ClubClaim, error) {
	return take[ClubClaim]("claim of file", claimCols, db.Conn(ctx, s.orm).Where("evidence_file_id = ?", fileID))
}

// DeleteClaim implements Store.
//
// cross-org: the KVKK erasure deletes the claimant's claims by ID.
func (s gormStore) DeleteClaim(ctx context.Context, id uuid.UUID) error {
	if err := db.Conn(ctx, s.orm).Exec(`DELETE FROM club_claims WHERE id = ?`, id).Error; err != nil {
		return wrap("delete claim", err)
	}
	return nil
}

func claimRows(op string, q *gorm.DB) ([]ClubClaim, error) {
	var out []ClubClaim
	if err := q.Scan(&out).Error; err != nil {
		return nil, wrap(op, err)
	}
	return out, nil
}

// decidedSQL selects the decided claims of the retention club_claim; the
// decision time is reviewed_at, or the last update of a withdrawal.
const decidedSQL = `status IN ('approved','rejected','withdrawn') AND COALESCE(reviewed_at, updated_at) < @cutoff`

// ClaimsDecidedBefore implements Store.
//
// cross-org: retention spans every claim.
func (s gormStore) ClaimsDecidedBefore(ctx context.Context, cutoff time.Time, limit int) ([]ClubClaim, error) {
	var rows []ClubClaim
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+claimCols+` FROM club_claims WHERE `+decidedSQL+`
		ORDER BY COALESCE(reviewed_at, updated_at), id LIMIT @limit FOR UPDATE SKIP LOCKED`,
		map[string]any{"cutoff": cutoff, "limit": limit}).Scan(&rows).Error
	if err != nil {
		return nil, wrap("claims decided before", err)
	}
	return rows, nil
}

// CountClaimsDecidedBefore implements Store.
//
// cross-org: retention spans every claim.
func (s gormStore) CountClaimsDecidedBefore(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM club_claims WHERE `+decidedSQL,
		map[string]any{"cutoff": cutoff}).Scan(&n).Error; err != nil {
		return 0, wrap("count claims decided before", err)
	}
	return int(n), nil
}
