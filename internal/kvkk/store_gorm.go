package kvkk

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/db"
)

// NewGormStore returns the PostgreSQL store over data_subject_requests
// (02 §16.1).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// constraints maps the 000088 CHECK constraints onto module errors; the
// service checks the same rules first, so these are defence in depth.
var constraints = db.Constraints{
	"data_subject_requests_requester_chk":          ErrInvalidRequest,
	"data_subject_requests_approvers_chk":          ErrState,
	"data_subject_requests_profiling_approval_chk": ErrSecondApprovalRequired,
	"data_subject_requests_completed_chk":          ErrState,
	"data_subject_requests_rejected_chk":           ErrInvalidRequest,
	"data_subject_requests_user_id_fkey":           ErrUnknownUser,
	"data_subject_requests_assigned_to_fkey":       ErrUnknownUser,
}

const dsrCols = `id, user_id, requester_name, requester_email, type, channel, status, details, received_at, due_at,
	identity_verified_at, assigned_to, first_approver, second_approver, response_summary, export_file_id,
	rejection_reason, completed_at, created_at, updated_at`

// dsrRow scans one data_subject_requests row.
type dsrRow struct {
	ID                 uuid.UUID  `gorm:"column:id"`
	UserID             *uuid.UUID `gorm:"column:user_id"`
	RequesterName      *string    `gorm:"column:requester_name"`
	RequesterEmail     *string    `gorm:"column:requester_email"`
	Type               string     `gorm:"column:type"`
	Channel            string     `gorm:"column:channel"`
	Status             string     `gorm:"column:status"`
	Details            *string    `gorm:"column:details"`
	ReceivedAt         time.Time  `gorm:"column:received_at"`
	DueAt              time.Time  `gorm:"column:due_at"`
	IdentityVerifiedAt *time.Time `gorm:"column:identity_verified_at"`
	AssignedTo         *uuid.UUID `gorm:"column:assigned_to"`
	FirstApprover      *uuid.UUID `gorm:"column:first_approver"`
	SecondApprover     *uuid.UUID `gorm:"column:second_approver"`
	ResponseSummary    *string    `gorm:"column:response_summary"`
	ExportFileID       *uuid.UUID `gorm:"column:export_file_id"`
	RejectionReason    *string    `gorm:"column:rejection_reason"`
	CompletedAt        *time.Time `gorm:"column:completed_at"`
	CreatedAt          time.Time  `gorm:"column:created_at;->"`
	UpdatedAt          time.Time  `gorm:"column:updated_at;->"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// dsr converts a scanned row.
//
// cross-org: data subject requests belong to a person, not an org.
func (r dsrRow) dsr() DSR {
	return DSR{
		ID: r.ID, UserID: r.UserID, RequesterName: str(r.RequesterName), RequesterEmail: str(r.RequesterEmail),
		Type: Type(r.Type), Channel: Channel(r.Channel), Status: Status(r.Status), Details: str(r.Details),
		ReceivedAt: r.ReceivedAt.UTC(), DueAt: r.DueAt.UTC(), IdentityVerifiedAt: db.UTCPtr(r.IdentityVerifiedAt),
		AssignedTo: r.AssignedTo, FirstApprover: r.FirstApprover, SecondApprover: r.SecondApprover,
		ResponseSummary: str(r.ResponseSummary), ExportFileID: r.ExportFileID, RejectionReason: str(r.RejectionReason),
		CompletedAt: db.UTCPtr(r.CompletedAt), CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
}

func dsrs(rows []dsrRow) []DSR {
	out := make([]DSR, len(rows))
	for i, r := range rows {
		out[i] = r.dsr()
	}
	return out
}

// Insert implements Store.
//
// cross-org: data subject requests belong to a person, not an org.
func (s gormStore) Insert(ctx context.Context, d DSR) error {
	err := db.Conn(ctx, s.orm).Exec(`INSERT INTO data_subject_requests (id, user_id, requester_name, requester_email, type,
		channel, status, details, received_at, due_at, identity_verified_at, created_at, updated_at)
		VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.UserID, db.Nullable(d.RequesterName), db.Nullable(d.RequesterEmail), string(d.Type), string(d.Channel),
		string(d.Status), db.Nullable(d.Details), d.ReceivedAt, d.DueAt, d.IdentityVerifiedAt, d.ReceivedAt, d.ReceivedAt).Error
	if err != nil {
		return fmt.Errorf("kvkk: insert request: %w", constraints.Translate(err))
	}
	return nil
}

// cross-org: data subject requests belong to a person, not an org.
func (s gormStore) one(ctx context.Context, sql string, args ...any) (DSR, error) {
	var rows []dsrRow
	if err := db.Conn(ctx, s.orm).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return DSR{}, fmt.Errorf("kvkk: read request: %w", err)
	}
	if len(rows) == 0 {
		return DSR{}, ErrNotFound
	}
	return rows[0].dsr(), nil
}

// Get implements Store.
//
// cross-org: data subject requests belong to a person, not an org.
func (s gormStore) Get(ctx context.Context, id uuid.UUID) (DSR, error) {
	return s.one(ctx, `SELECT `+dsrCols+` FROM data_subject_requests WHERE id = ?`, id)
}

// GetOwn implements Store.
//
// cross-org: data subject requests belong to a person, not an org.
func (s gormStore) GetOwn(ctx context.Context, id, userID uuid.UUID) (DSR, error) {
	return s.one(ctx, `SELECT `+dsrCols+` FROM data_subject_requests WHERE id = ? AND user_id = ?`, id, userID)
}

// Lock implements Store.
//
// cross-org: data subject requests belong to a person, not an org.
func (s gormStore) Lock(ctx context.Context, id uuid.UUID) (DSR, error) {
	return s.one(ctx, `SELECT `+dsrCols+` FROM data_subject_requests WHERE id = ? FOR UPDATE`, id)
}

// Update implements Store. updated_at is set by the touch trigger.
//
// cross-org: data subject requests belong to a person, not an org.
func (s gormStore) Update(ctx context.Context, d DSR) error {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE data_subject_requests SET status = ?, identity_verified_at = ?, assigned_to = ?,
		first_approver = ?, second_approver = ?, response_summary = ?, export_file_id = ?, rejection_reason = ?,
		completed_at = ? WHERE id = ?`,
		string(d.Status), d.IdentityVerifiedAt, d.AssignedTo, d.FirstApprover, d.SecondApprover,
		db.Nullable(d.ResponseSummary), d.ExportFileID, db.Nullable(d.RejectionReason), d.CompletedAt, d.ID)
	if res.Error != nil {
		return fmt.Errorf("kvkk: update request: %w", constraints.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// cross-org: data subject requests belong to a person, not an org.
func (s gormStore) many(ctx context.Context, sql string, args ...any) ([]DSR, error) {
	var rows []dsrRow
	if err := db.Conn(ctx, s.orm).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("kvkk: list requests: %w", err)
	}
	return dsrs(rows), nil
}

// ListOwn implements Store.
//
// cross-org: data subject requests belong to a person, not an org.
func (s gormStore) ListOwn(ctx context.Context, userID uuid.UUID, after *After, limit int) ([]DSR, error) {
	if after == nil {
		return s.many(ctx, `SELECT `+dsrCols+` FROM data_subject_requests WHERE user_id = ?
			ORDER BY received_at DESC, id DESC LIMIT ?`, userID, limit)
	}
	return s.many(ctx, `SELECT `+dsrCols+` FROM data_subject_requests WHERE user_id = ?
		AND (received_at, id) < ( ?, ?) ORDER BY received_at DESC, id DESC LIMIT ?`, userID, after.At, after.ID, limit)
}

// List implements Store.
//
// cross-org: the DPO queue spans every request.
func (s gormStore) List(ctx context.Context, f AdminFilter, after *After, limit int) ([]DSR, error) {
	q := db.Conn(ctx, s.orm).Table("data_subject_requests").Select(dsrCols)
	if len(f.Statuses) > 0 {
		st := make([]string, len(f.Statuses))
		for i, x := range f.Statuses {
			st[i] = string(x)
		}
		q = q.Where("status IN ?", st)
	}
	if f.Type != nil {
		q = q.Where("type = ?", string(*f.Type))
	}
	if f.DueBefore != nil {
		q = q.Where("due_at < ?", *f.DueBefore)
	}
	if after != nil {
		q = q.Where("(due_at, id) > ( ?, ?)", after.At, after.ID)
	}
	var rows []dsrRow
	if err := q.Order("due_at ASC, id ASC").Limit(limit).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("kvkk: list requests: %w", err)
	}
	return dsrs(rows), nil
}

// HasOpen implements Store.
//
// cross-org: data subject requests belong to a person, not an org.
func (s gormStore) HasOpen(ctx context.Context, userID uuid.UUID, t Type) (bool, error) {
	var n int64
	err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM data_subject_requests WHERE user_id = ? AND type = ?
		AND status NOT IN ('completed','rejected','cancelled')`, userID, string(t)).Scan(&n).Error
	if err != nil {
		return false, fmt.Errorf("kvkk: open requests: %w", err)
	}
	return n > 0, nil
}

// AllOf implements Store.
//
// cross-org: data subject requests belong to a person, not an org.
func (s gormStore) AllOf(ctx context.Context, userID uuid.UUID) ([]DSR, error) {
	return s.many(ctx, `SELECT `+dsrCols+` FROM data_subject_requests WHERE user_id = ? ORDER BY received_at, id`, userID)
}

// DueBy implements Store (data_subject_requests_open_idx).
//
// cross-org: the DPO deadline digest spans every request.
func (s gormStore) DueBy(ctx context.Context, cutoff time.Time, after *After, limit int) ([]DSR, error) {
	const open = `status NOT IN ('completed','rejected','cancelled')`
	if after == nil {
		return s.many(ctx, `SELECT `+dsrCols+` FROM data_subject_requests WHERE `+open+` AND due_at <= ?
			ORDER BY due_at, id LIMIT ?`, cutoff, limit)
	}
	return s.many(ctx, `SELECT `+dsrCols+` FROM data_subject_requests WHERE `+open+` AND due_at <= ?
		AND (due_at, id) > ( ?, ?) ORDER BY due_at, id LIMIT ?`, cutoff, after.At, after.ID, limit)
}

// ExportsCompletedBefore implements Store.
//
// cross-org: the export purge spans every request.
func (s gormStore) ExportsCompletedBefore(ctx context.Context, cutoff time.Time, afterID uuid.UUID, limit int) ([]DSR, error) {
	return s.many(ctx, `SELECT `+dsrCols+` FROM data_subject_requests WHERE export_file_id IS NOT NULL
		AND completed_at <= ? AND id > ? ORDER BY id LIMIT ?`, cutoff, afterID, limit)
}

// closedSQL is the close time of a request: completed_at (completed,
// rejected) or the last update of a cancelled one.
const closedSQL = `status IN ('completed','rejected','cancelled') AND export_file_id IS NULL
	AND COALESCE(completed_at, updated_at) < @cutoff`

// PurgeClosed implements Store.
//
// cross-org: retention spans every request.
func (s gormStore) PurgeClosed(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	args := map[string]any{"cutoff": cutoff, "limit": limit}
	if dryRun {
		var n int64
		if err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM data_subject_requests WHERE `+closedSQL, args).Scan(&n).Error; err != nil {
			return 0, db.Translate(err)
		}
		return int(n), nil
	}
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM data_subject_requests WHERE id IN (SELECT id FROM data_subject_requests
		WHERE `+closedSQL+` ORDER BY COALESCE(completed_at, updated_at), id LIMIT @limit FOR UPDATE SKIP LOCKED)`, args)
	if res.Error != nil {
		return 0, db.Translate(res.Error)
	}
	return int(res.RowsAffected), nil
}
