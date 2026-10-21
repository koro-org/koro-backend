package messaging

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/db"
)

// NewGormStore returns the PostgreSQL store over contact_requests and
// contact_blocks (02 §14).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

const requestCols = `id, org_id, sender_user_id, student_user_id, via, opportunity_post_id, subject, message, status,
	responded_at, expires_at, entitlement_usage_id, created_at, updated_at`

// constraints maps the named constraints of 02 §14 onto module errors.
// Unlisted CHECKs are 422 and FKs 409 (db.Translate).
var constraints = db.Constraints{
	"contact_requests_pending_uq":           ErrRequestPending,
	"contact_requests_org_id_org_kind_fkey": ErrNotFound,
}

func wrap(op string, err error) error {
	return fmt.Errorf("messaging: %s: %w", op, constraints.Translate(err))
}

// rows runs a request query.
//
// cross-org: an executor; every caller writes its own scope into sql.
func (s gormStore) rows(ctx context.Context, op, sql string, args map[string]any) ([]ContactRequest, error) {
	var out []ContactRequest
	if err := db.Conn(ctx, s.orm).Raw(sql, args).Scan(&out).Error; err != nil {
		return nil, wrap(op, err)
	}
	return out, nil
}

// one runs a request query expecting one row (ErrNotFound without).
//
// cross-org: an executor; every caller writes its own scope into sql.
func (s gormStore) one(ctx context.Context, op, sql string, args map[string]any) (ContactRequest, error) {
	rows, err := s.rows(ctx, op, sql, args)
	switch {
	case err != nil:
		return ContactRequest{}, err
	case len(rows) == 0:
		return ContactRequest{}, ErrNotFound
	}
	return rows[0], nil
}

// Insert implements Store. created_at and updated_at come from the column
// defaults (BE-069); expires_at is the service's business time.
//
// cross-org: org_id is the row's own scope (the composite FK binds it to a
// company org).
func (s gormStore) Insert(ctx context.Context, r ContactRequest) (ContactRequest, error) {
	if r.Status == "" {
		r.Status = StatusPending
	}
	return s.one(ctx, "insert request", `INSERT INTO contact_requests
		(id, org_id, sender_user_id, student_user_id, via, opportunity_post_id, subject, message, status, responded_at,
		 expires_at, entitlement_usage_id)
		VALUES ( @id, @org_id, @sender, @student, @via, @post, @subject, @message, @status, @responded_at, @expires_at,
		 @usage)
		RETURNING `+requestCols, map[string]any{
		"id": r.ID, "org_id": r.OrgID, "sender": r.SenderUserID, "student": r.StudentUserID, "via": string(r.Via),
		"post": r.OpportunityPostID, "subject": r.Subject, "message": r.Message, "status": string(r.Status),
		"responded_at": r.RespondedAt, "expires_at": r.ExpiresAt, "usage": r.EntitlementUsageID,
	})
}

// Get implements Store.
func (s gormStore) Get(ctx context.Context, orgID, id uuid.UUID) (ContactRequest, error) {
	if orgID == uuid.Nil() {
		return ContactRequest{}, fmt.Errorf("messaging: get request: %w", db.ErrMissingOrgScope)
	}
	return s.one(ctx, "get request", `SELECT `+requestCols+` FROM contact_requests
		WHERE id = @id AND org_id = @org_id`, map[string]any{"id": id, "org_id": orgID})
}

// GetForStudent implements Store.
//
// cross-org: the student's own request, addressed by its student (IDOR).
func (s gormStore) GetForStudent(ctx context.Context, studentID, id uuid.UUID) (ContactRequest, error) {
	return s.one(ctx, "get request of student", `SELECT `+requestCols+` FROM contact_requests
		WHERE id = @id AND student_user_id = @student`, map[string]any{"id": id, "student": studentID})
}

// Lock implements Store.
//
// cross-org: the caller read the request through its org or its student
// first; the lock is by ID.
func (s gormStore) Lock(ctx context.Context, id uuid.UUID) (ContactRequest, error) {
	return s.one(ctx, "lock request", `SELECT `+requestCols+` FROM contact_requests WHERE id = @id FOR UPDATE`,
		map[string]any{"id": id})
}

// Transition implements Store.
//
// cross-org: a state machine step of a request the caller locked (03
// §6.1-like conditional update; the from status is the guard).
func (s gormStore) Transition(ctx context.Context, id uuid.UUID, from, to Status, respondedAt *time.Time) (ContactRequest, bool, error) {
	rows, err := s.rows(ctx, "transition request", `UPDATE contact_requests
		SET status = @to, responded_at = @responded_at
		WHERE id = @id AND status = @from RETURNING `+requestCols, map[string]any{
		"to": string(to), "responded_at": respondedAt, "id": id, "from": string(from),
	})
	if err != nil || len(rows) == 0 {
		return ContactRequest{}, false, err
	}
	return rows[0], true, nil
}

// Pending implements Store.
func (s gormStore) Pending(ctx context.Context, orgID, studentID uuid.UUID) (ContactRequest, error) {
	if orgID == uuid.Nil() {
		return ContactRequest{}, fmt.Errorf("messaging: pending request: %w", db.ErrMissingOrgScope)
	}
	return s.one(ctx, "pending request", `SELECT `+requestCols+` FROM contact_requests
		WHERE org_id = @org_id AND student_user_id = @student AND status = 'pending'`,
		map[string]any{"org_id": orgID, "student": studentID})
}

// DeclinedSince implements Store.
func (s gormStore) DeclinedSince(ctx context.Context, orgID, studentID uuid.UUID, since time.Time) (bool, error) {
	var n int64
	err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM contact_requests
		WHERE org_id = @org_id AND student_user_id = @student AND status = 'declined' AND responded_at > @since`,
		map[string]any{"org_id": orgID, "student": studentID, "since": since}).Scan(&n).Error
	if err != nil {
		return false, wrap("declined since", err)
	}
	return n > 0, nil
}

// listFilter adds the effective status filter and the keyset position of
// a request list.
func listFilter(args map[string]any, q ListQuery) string {
	sql := ""
	switch q.Status {
	case "":
	case StatusPending:
		sql += ` AND status = 'pending' AND expires_at > @now`
		args["now"] = q.Now
	case StatusExpired:
		sql += ` AND (status = 'expired' OR (status = 'pending' AND expires_at <= @now))`
		args["now"] = q.Now
	default:
		sql += ` AND status = @status`
		args["status"] = string(q.Status)
	}
	if q.After != nil {
		sql += ` AND id < @after`
		args["after"] = *q.After
	}
	return sql
}

// ListByOrg implements Store.
func (s gormStore) ListByOrg(ctx context.Context, orgID uuid.UUID, q ListQuery) ([]ContactRequest, error) {
	if orgID == uuid.Nil() {
		return nil, fmt.Errorf("messaging: list requests: %w", db.ErrMissingOrgScope)
	}
	args := map[string]any{"org_id": orgID, "limit": q.Limit}
	return s.rows(ctx, "list requests", `SELECT `+requestCols+` FROM contact_requests
		WHERE org_id = @org_id`+listFilter(args, q)+` ORDER BY id DESC LIMIT @limit`, args)
}

// ListByStudent implements Store.
//
// cross-org: the student's own requests of every company (listMyContactRequests).
func (s gormStore) ListByStudent(ctx context.Context, studentID uuid.UUID, q ListQuery) ([]ContactRequest, error) {
	args := map[string]any{"student": studentID, "limit": q.Limit}
	return s.rows(ctx, "list requests of student", `SELECT `+requestCols+` FROM contact_requests
		WHERE student_user_id = @student`+listFilter(args, q)+` ORDER BY id DESC LIMIT @limit`, args)
}

// PendingOfCompany implements Store.
func (s gormStore) PendingOfCompany(ctx context.Context, orgID, studentID uuid.UUID) ([]ContactRequest, error) {
	return s.rows(ctx, "pending requests of company", `SELECT `+requestCols+` FROM contact_requests
		WHERE org_id = @org_id AND student_user_id = @student AND status = 'pending' ORDER BY id FOR UPDATE`,
		map[string]any{"org_id": orgID, "student": studentID})
}

// DueForExpiry implements Store: SKIP LOCKED, so two workers never expire
// the same request (and a request a student is answering waits for the
// next run).
//
// cross-org: the hourly expiry job of every company (01 §7).
func (s gormStore) DueForExpiry(ctx context.Context, now time.Time, limit int) ([]ContactRequest, error) {
	return s.rows(ctx, "requests due for expiry", `SELECT `+requestCols+` FROM contact_requests
		WHERE status = 'pending' AND expires_at <= @now ORDER BY expires_at, id LIMIT @limit FOR UPDATE SKIP LOCKED`,
		map[string]any{"now": now, "limit": limit})
}

// InsertBlock implements Store.
//
// cross-org: the student's own block (no org scope: 02 §14.2).
func (s gormStore) InsertBlock(ctx context.Context, studentID, orgID uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`INSERT INTO contact_blocks (student_user_id, company_org_id)
		VALUES ( @student, @company) ON CONFLICT (student_user_id, company_org_id) DO NOTHING`,
		map[string]any{"student": studentID, "company": orgID})
	if res.Error != nil {
		return false, wrap("insert block", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// DeleteBlock implements Store.
//
// cross-org: the student's own block.
func (s gormStore) DeleteBlock(ctx context.Context, studentID, orgID uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM contact_blocks WHERE student_user_id = @student AND company_org_id = @company`,
		map[string]any{"student": studentID, "company": orgID})
	if res.Error != nil {
		return false, wrap("delete block", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// Blocked implements Store.
//
// cross-org: the student's own block.
func (s gormStore) Blocked(ctx context.Context, studentID, orgID uuid.UUID) (bool, error) {
	var n int64
	err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM contact_blocks WHERE student_user_id = @student AND company_org_id = @company`,
		map[string]any{"student": studentID, "company": orgID}).Scan(&n).Error
	if err != nil {
		return false, wrap("blocked", err)
	}
	return n > 0, nil
}

// Blocks implements Store.
//
// cross-org: the student's own blocks of every company.
func (s gormStore) Blocks(ctx context.Context, studentID uuid.UUID, after *BlockKey, limit int) ([]Block, error) {
	args := map[string]any{"student": studentID, "limit": limit}
	where := ""
	if after != nil {
		args["after_at"], args["after_org"] = after.CreatedAt, after.OrgID
		where = ` AND (created_at < @after_at OR (created_at = @after_at AND company_org_id < @after_org))`
	}
	var out []Block
	err := db.Conn(ctx, s.orm).Raw(`SELECT student_user_id, company_org_id, created_at FROM contact_blocks
		WHERE student_user_id = @student`+where+` ORDER BY created_at DESC, company_org_id DESC LIMIT @limit`, args).
		Scan(&out).Error
	if err != nil {
		return nil, wrap("blocks", err)
	}
	return out, nil
}

// ReceivedBy implements Store.
//
// cross-org: the data subject's requests of every company (KVKK export).
func (s gormStore) ReceivedBy(ctx context.Context, studentID uuid.UUID) ([]ContactRequest, error) {
	return s.rows(ctx, "requests received", `SELECT `+requestCols+` FROM contact_requests
		WHERE student_user_id = @student ORDER BY id`, map[string]any{"student": studentID})
}

// SentBy implements Store.
//
// cross-org: the data subject's requests sent for any company (KVKK export).
func (s gormStore) SentBy(ctx context.Context, senderID uuid.UUID) ([]ContactRequest, error) {
	return s.rows(ctx, "requests sent", `SELECT `+requestCols+` FROM contact_requests
		WHERE sender_user_id = @sender ORDER BY id`, map[string]any{"sender": senderID})
}

// DeleteReceived implements Store.
//
// cross-org: the data subject's requests of every company (account erasure).
func (s gormStore) DeleteReceived(ctx context.Context, studentID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM contact_requests WHERE student_user_id = @student`,
		map[string]any{"student": studentID})
	if res.Error != nil {
		return 0, wrap("delete received", res.Error)
	}
	return int(res.RowsAffected), nil
}

// DeleteBlocks implements Store.
//
// cross-org: the data subject's blocks (account erasure).
func (s gormStore) DeleteBlocks(ctx context.Context, studentID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM contact_blocks WHERE student_user_id = @student`,
		map[string]any{"student": studentID})
	if res.Error != nil {
		return 0, wrap("delete blocks", res.Error)
	}
	return int(res.RowsAffected), nil
}

// CountClosedBefore implements Store.
//
// cross-org: the retention sweep of every company (contact_request_closed).
func (s gormStore) CountClosedBefore(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM contact_requests WHERE status <> 'pending' AND updated_at < @cutoff`,
		map[string]any{"cutoff": cutoff}).Scan(&n).Error
	if err != nil {
		return 0, wrap("count closed", err)
	}
	return int(n), nil
}

// PurgeClosed implements Store.
//
// cross-org: the retention sweep of every company (contact_request_closed).
func (s gormStore) PurgeClosed(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM contact_requests WHERE id IN (
		SELECT id FROM contact_requests WHERE status <> 'pending' AND updated_at < @cutoff
		ORDER BY updated_at, id LIMIT @limit FOR UPDATE SKIP LOCKED)`, map[string]any{"cutoff": cutoff, "limit": limit})
	if res.Error != nil {
		return 0, wrap("purge closed", res.Error)
	}
	return int(res.RowsAffected), nil
}
