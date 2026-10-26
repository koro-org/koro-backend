package kvkk

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/civil"
	"koro/internal/platform/db"
)

// NewGormRecords returns the PostgreSQL RecordStore over retention_policies,
// destruction_logs, breach_incidents and subprocessors (02 §16.2–16.5).
func NewGormRecords(orm *gorm.DB) RecordStore { return gormRecords{orm: orm} }

type gormRecords struct{ orm *gorm.DB }

// recordConstraints maps the 000089–000093 constraints onto module errors;
// the service checks the same rules first (defence in depth).
var recordConstraints = db.Constraints{
	"retention_policies_approval_chk":        ErrInvalidRequest,
	"retention_policies_retention_chk":       ErrInvalidRequest,
	"retention_policies_method_chk":          ErrInvalidRequest,
	"retention_policies_updated_by_fkey":     ErrUnknownUser,
	"destruction_logs_policy_code_fkey":      ErrInvalidRequest,
	"destruction_logs_method_chk":            ErrInvalidRequest,
	"destruction_logs_record_count_chk":      ErrInvalidRequest,
	"breach_incidents_title_chk":             ErrInvalidRequest,
	"breach_incidents_description_chk":       ErrInvalidRequest,
	"breach_incidents_status_chk":            ErrInvalidRequest,
	"breach_incidents_closed_chk":            ErrState,
	"breach_incidents_affected_estimate_chk": ErrInvalidRequest,
	"breach_incidents_owner_user_id_fkey":    ErrUnknownUser,
	"subprocessors_name_uq":                  ErrDuplicateSubprocessor,
	"subprocessors_country_code_chk":         ErrInvalidRequest,
	"subprocessors_transfer_mechanism_chk":   ErrInvalidRequest,
	"subprocessors_domestic_chk":             ErrInvalidRequest,
}

// ---------------------------------------------------------------- policies

const policyCols = `code, owner_module, target, extract(year FROM retention)::int AS years,
	extract(month FROM retention)::int AS months, extract(day FROM retention)::int AS days,
	extract(hour FROM retention)::int AS hours, anchor, method, legal_basis, legal_approved, approved_ref, enabled,
	description_tr, updated_by, created_at, updated_at`

type policyRow struct {
	Code          string     `gorm:"column:code"`
	OwnerModule   string     `gorm:"column:owner_module"`
	Target        string     `gorm:"column:target"`
	Years         int        `gorm:"column:years"`
	Months        int        `gorm:"column:months"`
	Days          int        `gorm:"column:days"`
	Hours         int        `gorm:"column:hours"`
	Anchor        string     `gorm:"column:anchor"`
	Method        string     `gorm:"column:method"`
	LegalBasis    string     `gorm:"column:legal_basis"`
	LegalApproved bool       `gorm:"column:legal_approved"`
	ApprovedRef   *string    `gorm:"column:approved_ref"`
	Enabled       bool       `gorm:"column:enabled"`
	DescriptionTR string     `gorm:"column:description_tr"`
	UpdatedBy     *uuid.UUID `gorm:"column:updated_by"`
	CreatedAt     time.Time  `gorm:"column:created_at;->"`
	UpdatedAt     time.Time  `gorm:"column:updated_at;->"`
}

func (r policyRow) policy() Policy {
	return Policy{Code: r.Code, OwnerModule: r.OwnerModule, Target: r.Target,
		Period: Period{Years: r.Years, Months: r.Months, Days: r.Days, Hours: r.Hours}, Anchor: r.Anchor, Method: r.Method,
		LegalBasis: r.LegalBasis, LegalApproved: r.LegalApproved, ApprovedRef: str(r.ApprovedRef), Enabled: r.Enabled,
		DescriptionTR: r.DescriptionTR, UpdatedBy: r.UpdatedBy, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
}

// cross-org: retention policies are platform settings.
func (s gormRecords) policies(ctx context.Context, sql string, args ...any) ([]Policy, error) {
	var rows []policyRow
	if err := db.Conn(ctx, s.orm).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("kvkk: read retention policies: %w", err)
	}
	out := make([]Policy, len(rows))
	for i, r := range rows {
		out[i] = r.policy()
	}
	return out, nil
}

// Policies implements RecordStore.
//
// cross-org: retention policies are platform settings.
func (s gormRecords) Policies(ctx context.Context) ([]Policy, error) {
	return s.policies(ctx, `SELECT `+policyCols+` FROM retention_policies ORDER BY code`)
}

// cross-org: retention policies are platform settings.
func (s gormRecords) onePolicy(ctx context.Context, sql string, args ...any) (Policy, error) {
	ps, err := s.policies(ctx, sql, args...)
	if err != nil {
		return Policy{}, err
	}
	if len(ps) == 0 {
		return Policy{}, ErrNotFound
	}
	return ps[0], nil
}

// Policy implements RecordStore.
//
// cross-org: retention policies are platform settings.
func (s gormRecords) Policy(ctx context.Context, code string) (Policy, error) {
	return s.onePolicy(ctx, `SELECT `+policyCols+` FROM retention_policies WHERE code = ?`, code)
}

// LockPolicy implements RecordStore.
//
// cross-org: retention policies are platform settings.
func (s gormRecords) LockPolicy(ctx context.Context, code string) (Policy, error) {
	return s.onePolicy(ctx, `SELECT `+policyCols+` FROM retention_policies WHERE code = ? FOR UPDATE`, code)
}

// UpdatePolicy implements RecordStore. updated_at is set by the trigger.
//
// cross-org: retention policies are platform settings.
func (s gormRecords) UpdatePolicy(ctx context.Context, p Policy) error {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE retention_policies SET retention = make_interval( ?, ?, 0, ?, ?), method = ?,
		enabled = ?, legal_approved = ?, approved_ref = ?, updated_by = ? WHERE code = ?`,
		p.Period.Years, p.Period.Months, p.Period.Days, p.Period.Hours, p.Method, p.Enabled, p.LegalApproved,
		db.Nullable(p.ApprovedRef), p.UpdatedBy, p.Code)
	if res.Error != nil {
		return fmt.Errorf("kvkk: update retention policy: %w", recordConstraints.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- destruction logs

const logCols = `id, policy_code, run_id, module, target, method, record_count, period_cutoff, started_at, finished_at,
	details, created_at`

type logRow struct {
	ID           uuid.UUID `gorm:"column:id"`
	PolicyCode   string    `gorm:"column:policy_code"`
	RunID        uuid.UUID `gorm:"column:run_id"`
	Module       string    `gorm:"column:module"`
	Target       string    `gorm:"column:target"`
	Method       string    `gorm:"column:method"`
	RecordCount  int       `gorm:"column:record_count"`
	PeriodCutoff time.Time `gorm:"column:period_cutoff"`
	StartedAt    time.Time `gorm:"column:started_at"`
	FinishedAt   time.Time `gorm:"column:finished_at"`
	Details      []byte    `gorm:"column:details"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

func (r logRow) log() (DestructionLog, error) {
	l := DestructionLog{ID: r.ID, PolicyCode: r.PolicyCode, RunID: r.RunID, Module: r.Module, Target: r.Target,
		Method: r.Method, RecordCount: r.RecordCount, PeriodCutoff: r.PeriodCutoff.UTC(), StartedAt: r.StartedAt.UTC(),
		FinishedAt: r.FinishedAt.UTC(), CreatedAt: r.CreatedAt.UTC(), Details: map[string]any{}}
	if len(r.Details) > 0 {
		if err := json.Unmarshal(r.Details, &l.Details); err != nil {
			return DestructionLog{}, fmt.Errorf("kvkk: destruction log details: %w", err)
		}
	}
	return l, nil
}

// InsertDestructionLog implements RecordStore.
//
// cross-org: the destruction record is platform-wide.
func (s gormRecords) InsertDestructionLog(ctx context.Context, l DestructionLog) error {
	details := l.Details
	if details == nil {
		details = map[string]any{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("kvkk: destruction log details: %w", err)
	}
	err = db.Conn(ctx, s.orm).Exec(`INSERT INTO destruction_logs (id, policy_code, run_id, module, target, method, record_count,
		period_cutoff, started_at, finished_at, details) VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?::jsonb)`,
		l.ID, l.PolicyCode, l.RunID, l.Module, l.Target, l.Method, l.RecordCount, l.PeriodCutoff, l.StartedAt, l.FinishedAt,
		string(raw)).Error
	if err != nil {
		return fmt.Errorf("kvkk: insert destruction log: %w", recordConstraints.Translate(err))
	}
	return nil
}

// cross-org: the destruction record is platform-wide.
func (s gormRecords) logs(q *gorm.DB) ([]DestructionLog, error) {
	var rows []logRow
	if err := q.Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("kvkk: read destruction logs: %w", err)
	}
	out := make([]DestructionLog, len(rows))
	for i, r := range rows {
		l, err := r.log()
		if err != nil {
			return nil, err
		}
		out[i] = l
	}
	return out, nil
}

// DestructionLogs implements RecordStore (destruction_logs_policy_idx).
//
// cross-org: the destruction record is platform-wide.
func (s gormRecords) DestructionLogs(ctx context.Context, f DestructionFilter, after *After, limit int) ([]DestructionLog, error) {
	q := db.Conn(ctx, s.orm).Table("destruction_logs").Select(logCols)
	if f.PolicyCode != "" {
		q = q.Where("policy_code = ?", f.PolicyCode)
	}
	if f.From != nil {
		q = q.Where("created_at >= ?", *f.From)
	}
	if f.To != nil {
		q = q.Where("created_at < ?", *f.To)
	}
	if after != nil {
		q = q.Where("(created_at, id) < ( ?, ?)", after.At, after.ID)
	}
	return s.logs(q.Order("created_at DESC, id DESC").Limit(limit))
}

// DestructionLogsOfRun implements RecordStore.
//
// cross-org: the destruction record is platform-wide.
func (s gormRecords) DestructionLogsOfRun(ctx context.Context, runID uuid.UUID) ([]DestructionLog, error) {
	return s.logs(db.Conn(ctx, s.orm).Raw(`SELECT `+logCols+` FROM destruction_logs WHERE run_id = ? ORDER BY created_at, id`, runID))
}

// ---------------------------------------------------------------- breaches

const breachCols = `id, title, detected_at, authority_deadline_at, controller_notified_at, authority_notified_at,
	data_categories, affected_estimate, description, status, owner_user_id, closed_at, created_at, updated_at`

type breachRow struct {
	ID                   uuid.UUID    `gorm:"column:id"`
	Title                string       `gorm:"column:title"`
	DetectedAt           time.Time    `gorm:"column:detected_at"`
	AuthorityDeadlineAt  time.Time    `gorm:"column:authority_deadline_at"`
	ControllerNotifiedAt *time.Time   `gorm:"column:controller_notified_at"`
	AuthorityNotifiedAt  *time.Time   `gorm:"column:authority_notified_at"`
	DataCategories       db.TextArray `gorm:"column:data_categories"`
	AffectedEstimate     *int         `gorm:"column:affected_estimate"`
	Description          string       `gorm:"column:description"`
	Status               string       `gorm:"column:status"`
	OwnerUserID          uuid.UUID    `gorm:"column:owner_user_id"`
	ClosedAt             *time.Time   `gorm:"column:closed_at"`
	CreatedAt            time.Time    `gorm:"column:created_at;->"`
	UpdatedAt            time.Time    `gorm:"column:updated_at;->"`
}

func (r breachRow) breach() Breach {
	return Breach{ID: r.ID, Title: r.Title, DetectedAt: r.DetectedAt.UTC(), AuthorityDeadlineAt: r.AuthorityDeadlineAt.UTC(),
		ControllerNotifiedAt: db.UTCPtr(r.ControllerNotifiedAt), AuthorityNotifiedAt: db.UTCPtr(r.AuthorityNotifiedAt),
		DataCategories: []string(r.DataCategories), AffectedEstimate: r.AffectedEstimate, Description: r.Description,
		Status: BreachStatus(r.Status), OwnerUserID: r.OwnerUserID, ClosedAt: db.UTCPtr(r.ClosedAt),
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
}

// InsertBreach implements RecordStore.
//
// cross-org: the breach register is platform-wide.
func (s gormRecords) InsertBreach(ctx context.Context, b Breach) error {
	err := db.Conn(ctx, s.orm).Exec(`INSERT INTO breach_incidents (id, title, detected_at, authority_deadline_at,
		controller_notified_at, authority_notified_at, data_categories, affected_estimate, description, status, owner_user_id,
		closed_at) VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.Title, b.DetectedAt, b.AuthorityDeadlineAt, b.ControllerNotifiedAt, b.AuthorityNotifiedAt,
		db.TextArray(b.DataCategories), b.AffectedEstimate, b.Description, string(b.Status), b.OwnerUserID, b.ClosedAt).Error
	if err != nil {
		return fmt.Errorf("kvkk: insert breach: %w", recordConstraints.Translate(err))
	}
	return nil
}

// cross-org: the breach register is platform-wide.
func (s gormRecords) breaches(ctx context.Context, sql string, args ...any) ([]Breach, error) {
	var rows []breachRow
	if err := db.Conn(ctx, s.orm).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("kvkk: read breaches: %w", err)
	}
	out := make([]Breach, len(rows))
	for i, r := range rows {
		out[i] = r.breach()
	}
	return out, nil
}

// cross-org: the breach register is platform-wide.
func (s gormRecords) oneBreach(ctx context.Context, sql string, args ...any) (Breach, error) {
	bs, err := s.breaches(ctx, sql, args...)
	if err != nil {
		return Breach{}, err
	}
	if len(bs) == 0 {
		return Breach{}, ErrNotFound
	}
	return bs[0], nil
}

// Breach implements RecordStore.
//
// cross-org: the breach register is platform-wide.
func (s gormRecords) Breach(ctx context.Context, id uuid.UUID) (Breach, error) {
	return s.oneBreach(ctx, `SELECT `+breachCols+` FROM breach_incidents WHERE id = ?`, id)
}

// LockBreach implements RecordStore.
//
// cross-org: the breach register is platform-wide.
func (s gormRecords) LockBreach(ctx context.Context, id uuid.UUID) (Breach, error) {
	return s.oneBreach(ctx, `SELECT `+breachCols+` FROM breach_incidents WHERE id = ? FOR UPDATE`, id)
}

// UpdateBreach implements RecordStore. updated_at is set by the trigger.
//
// cross-org: the breach register is platform-wide.
func (s gormRecords) UpdateBreach(ctx context.Context, b Breach) error {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE breach_incidents SET title = ?, detected_at = ?, authority_deadline_at = ?,
		controller_notified_at = ?, authority_notified_at = ?, data_categories = ?, affected_estimate = ?, description = ?,
		status = ?, owner_user_id = ?, closed_at = ? WHERE id = ?`,
		b.Title, b.DetectedAt, b.AuthorityDeadlineAt, b.ControllerNotifiedAt, b.AuthorityNotifiedAt,
		db.TextArray(b.DataCategories), b.AffectedEstimate, b.Description, string(b.Status), b.OwnerUserID, b.ClosedAt, b.ID)
	if res.Error != nil {
		return fmt.Errorf("kvkk: update breach: %w", recordConstraints.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// Breaches implements RecordStore.
//
// cross-org: the breach register is platform-wide.
func (s gormRecords) Breaches(ctx context.Context, status *BreachStatus, after *After, limit int) ([]Breach, error) {
	sql, args := `SELECT `+breachCols+` FROM breach_incidents WHERE true`, []any{}
	if status != nil {
		sql, args = sql+` AND status = ?`, append(args, string(*status))
	}
	if after != nil {
		sql, args = sql+` AND (detected_at, id) < ( ?, ?)`, append(args, after.At, after.ID)
	}
	sql += ` ORDER BY detected_at DESC, id DESC`
	if limit > 0 {
		sql, args = sql+` LIMIT ?`, append(args, limit)
	}
	return s.breaches(ctx, sql, args...)
}

// ---------------------------------------------------------------- subprocessors

const subCols = `id, name, service, data_categories, region, country_code, transfer_mechanism, contract_signed_at,
	authority_notified_at, active, notes, created_at, updated_at`

type subRow struct {
	ID                  uuid.UUID    `gorm:"column:id"`
	Name                string       `gorm:"column:name"`
	Service             string       `gorm:"column:service"`
	DataCategories      db.TextArray `gorm:"column:data_categories"`
	Region              string       `gorm:"column:region"`
	CountryCode         string       `gorm:"column:country_code"`
	TransferMechanism   string       `gorm:"column:transfer_mechanism"`
	ContractSignedAt    *civil.Date  `gorm:"column:contract_signed_at"`
	AuthorityNotifiedAt *civil.Date  `gorm:"column:authority_notified_at"`
	Active              bool         `gorm:"column:active"`
	Notes               *string      `gorm:"column:notes"`
	CreatedAt           time.Time    `gorm:"column:created_at;->"`
	UpdatedAt           time.Time    `gorm:"column:updated_at;->"`
}

func (r subRow) sub() Subprocessor {
	return Subprocessor{ID: r.ID, Name: r.Name, Service: r.Service, DataCategories: []string(r.DataCategories),
		Region: r.Region, CountryCode: r.CountryCode, TransferMechanism: r.TransferMechanism,
		ContractSignedAt: r.ContractSignedAt, AuthorityNotifiedAt: r.AuthorityNotifiedAt, Active: r.Active,
		Notes: str(r.Notes), CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
}

// InsertSubprocessor implements RecordStore.
//
// cross-org: the sub-processor inventory is platform-wide.
func (s gormRecords) InsertSubprocessor(ctx context.Context, x Subprocessor) error {
	err := db.Conn(ctx, s.orm).Exec(`INSERT INTO subprocessors (id, name, service, data_categories, region, country_code,
		transfer_mechanism, contract_signed_at, authority_notified_at, active, notes) VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		x.ID, x.Name, x.Service, db.TextArray(x.DataCategories), x.Region, x.CountryCode, x.TransferMechanism,
		x.ContractSignedAt, x.AuthorityNotifiedAt, x.Active, db.Nullable(x.Notes)).Error
	if err != nil {
		return fmt.Errorf("kvkk: insert sub-processor: %w", recordConstraints.Translate(err))
	}
	return nil
}

// cross-org: the sub-processor inventory is platform-wide.
func (s gormRecords) subs(ctx context.Context, sql string, args ...any) ([]Subprocessor, error) {
	var rows []subRow
	if err := db.Conn(ctx, s.orm).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("kvkk: read sub-processors: %w", err)
	}
	out := make([]Subprocessor, len(rows))
	for i, r := range rows {
		out[i] = r.sub()
	}
	return out, nil
}

// cross-org: the sub-processor inventory is platform-wide.
func (s gormRecords) oneSub(ctx context.Context, sql string, args ...any) (Subprocessor, error) {
	xs, err := s.subs(ctx, sql, args...)
	if err != nil {
		return Subprocessor{}, err
	}
	if len(xs) == 0 {
		return Subprocessor{}, ErrNotFound
	}
	return xs[0], nil
}

// Subprocessor implements RecordStore.
//
// cross-org: the sub-processor inventory is platform-wide.
func (s gormRecords) Subprocessor(ctx context.Context, id uuid.UUID) (Subprocessor, error) {
	return s.oneSub(ctx, `SELECT `+subCols+` FROM subprocessors WHERE id = ?`, id)
}

// LockSubprocessor implements RecordStore.
//
// cross-org: the sub-processor inventory is platform-wide.
func (s gormRecords) LockSubprocessor(ctx context.Context, id uuid.UUID) (Subprocessor, error) {
	return s.oneSub(ctx, `SELECT `+subCols+` FROM subprocessors WHERE id = ? FOR UPDATE`, id)
}

// UpdateSubprocessor implements RecordStore. updated_at is set by the
// trigger.
//
// cross-org: the sub-processor inventory is platform-wide.
func (s gormRecords) UpdateSubprocessor(ctx context.Context, x Subprocessor) error {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE subprocessors SET name = ?, service = ?, data_categories = ?, region = ?,
		country_code = ?, transfer_mechanism = ?, contract_signed_at = ?, authority_notified_at = ?, active = ?, notes = ?
		WHERE id = ?`,
		x.Name, x.Service, db.TextArray(x.DataCategories), x.Region, x.CountryCode, x.TransferMechanism, x.ContractSignedAt,
		x.AuthorityNotifiedAt, x.Active, db.Nullable(x.Notes), x.ID)
	if res.Error != nil {
		return fmt.Errorf("kvkk: update sub-processor: %w", recordConstraints.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// Subprocessors implements RecordStore.
//
// cross-org: the sub-processor inventory is platform-wide.
func (s gormRecords) Subprocessors(ctx context.Context, active *bool) ([]Subprocessor, error) {
	if active != nil {
		return s.subs(ctx, `SELECT `+subCols+` FROM subprocessors WHERE active = ? ORDER BY name, id`, *active)
	}
	return s.subs(ctx, `SELECT `+subCols+` FROM subprocessors ORDER BY name, id`)
}

// PurgeDestructionLogs implements RecordStore.
func (s gormRecords) PurgeDestructionLogs(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT destruction_logs_purge( ?, ?, ?)`, cutoff, max(limit, 1), dryRun).Scan(&n).Error; err != nil {
		return 0, db.Translate(err)
	}
	return int(n), nil
}

// PurgeBreaches implements RecordStore.
func (s gormRecords) PurgeBreaches(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	args := map[string]any{"cutoff": cutoff, "limit": limit}
	if dryRun {
		var n int64
		if err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM breach_incidents WHERE status = 'closed' AND closed_at < @cutoff`, args).
			Scan(&n).Error; err != nil {
			return 0, db.Translate(err)
		}
		return int(n), nil
	}
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM breach_incidents WHERE id IN (SELECT id FROM breach_incidents
		WHERE status = 'closed' AND closed_at < @cutoff ORDER BY closed_at, id LIMIT @limit FOR UPDATE SKIP LOCKED)`, args)
	if res.Error != nil {
		return 0, db.Translate(res.Error)
	}
	return int(res.RowsAffected), nil
}
