package consent

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
)

// NewGormStore returns the PostgreSQL store over consent_purposes,
// notice_versions, consents (+ the consent_current view) and
// iys_submissions (02 §4).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// noticeConstraints maps the notice_versions constraints to module errors.
var noticeConstraints = db.Constraints{
	"notice_versions_version_uq":        ErrNoticeVersionTaken,
	"notice_versions_current_uq":        ErrNoticeState,
	"notice_versions_purpose_code_fkey": ErrUnknownPurpose,
}

// consentCols are the selected consents columns; ip is read as host text
// (the canonical form the in-memory store keeps).
const consentCols = `id, user_id, purpose_code, action, notice_version_id, text_sha256, scope_type, scope_id,
	channel, host(ip) AS ip, user_agent, occurred_at, created_at`

// consentRow scans one consents (or consent_current) row.
type consentRow struct {
	ID              uuid.UUID  `gorm:"column:id"`
	UserID          uuid.UUID  `gorm:"column:user_id"`
	PurposeCode     string     `gorm:"column:purpose_code"`
	Action          string     `gorm:"column:action"`
	NoticeVersionID *uuid.UUID `gorm:"column:notice_version_id"`
	TextSHA256      []byte     `gorm:"column:text_sha256"`
	ScopeType       string     `gorm:"column:scope_type"`
	ScopeID         *uuid.UUID `gorm:"column:scope_id"`
	Channel         string     `gorm:"column:channel"`
	IP              *string    `gorm:"column:ip"`
	UserAgent       *string    `gorm:"column:user_agent"`
	OccurredAt      time.Time  `gorm:"column:occurred_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
}

// toConsent converts a scanned row.
func toConsent(r consentRow) Consent {
	c := Consent{
		ID: r.ID, UserID: r.UserID, PurposeCode: Purpose(r.PurposeCode), Action: Action(r.Action),
		NoticeVersionID: r.NoticeVersionID, TextSHA256: r.TextSHA256, ScopeType: ScopeType(r.ScopeType),
		ScopeID: r.ScopeID, Channel: Channel(r.Channel), OccurredAt: r.OccurredAt.UTC(), CreatedAt: r.CreatedAt.UTC(),
	}
	if r.IP != nil {
		c.IP = canonicalIP(*r.IP)
	}
	if r.UserAgent != nil {
		c.UserAgent = *r.UserAgent
	}
	return c
}

func consents(rows []consentRow) []Consent {
	out := make([]Consent, len(rows))
	for i, r := range rows {
		out[i] = toConsent(r)
	}
	return out
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// canonicalIP returns the canonical text of an IP address ("" if s is not
// one), as PostgreSQL's host(inet) prints a host address.
func canonicalIP(s string) string {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return a.Unmap().WithZone("").String()
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// noticeUTC normalises the timestamps of a scanned notice.
func noticeUTC(n Notice) Notice {
	n.PublishedAt, n.RetiredAt, n.CreatedAt = utcPtr(n.PublishedAt), utcPtr(n.RetiredAt), n.CreatedAt.UTC()
	return n
}

// Purposes implements Store.
//
// cross-org: platform reference table.
func (s gormStore) Purposes(ctx context.Context) ([]PurposeDef, error) {
	var out []PurposeDef
	if err := db.Conn(ctx, s.orm).Order("code").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("consent: purposes: %w", err)
	}
	for i := range out {
		out[i].CreatedAt = out[i].CreatedAt.UTC()
	}
	return out, nil
}

// Purpose implements Store.
//
// cross-org: platform reference table.
func (s gormStore) Purpose(ctx context.Context, code Purpose) (PurposeDef, error) {
	return s.purpose(ctx, code, false)
}

// LockPurpose implements Store.
//
// cross-org: platform reference table.
func (s gormStore) LockPurpose(ctx context.Context, code Purpose) (PurposeDef, error) {
	return s.purpose(ctx, code, true)
}

// cross-org: platform reference table.
func (s gormStore) purpose(ctx context.Context, code Purpose, lock bool) (PurposeDef, error) {
	q := db.Conn(ctx, s.orm)
	if lock {
		q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	var p PurposeDef
	if err := q.Where("code = ?", string(code)).Take(&p).Error; err != nil {
		if db.IsNotFound(err) {
			return PurposeDef{}, ErrUnknownPurpose
		}
		return PurposeDef{}, fmt.Errorf("consent: purpose: %w", err)
	}
	p.CreatedAt = p.CreatedAt.UTC()
	return p, nil
}

// UpdatePurpose implements Store.
//
// cross-org: platform reference table.
func (s gormStore) UpdatePurpose(ctx context.Context, p PurposeDef) error {
	res := db.Conn(ctx, s.orm).Model(&PurposeDef{}).Where("code = ?", string(p.Code)).
		Updates(map[string]any{"description_tr": p.DescriptionTR, "active": p.Active})
	if res.Error != nil {
		return fmt.Errorf("consent: update purpose: %w", db.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrUnknownPurpose
	}
	return nil
}

// InsertNotice implements Store.
//
// cross-org: platform-wide texts.
func (s gormStore) InsertNotice(ctx context.Context, n Notice) error {
	n.PublishedAt, n.RetiredAt = nil, nil
	if err := db.Conn(ctx, s.orm).Create(&n).Error; err != nil {
		return fmt.Errorf("consent: insert notice: %w", noticeConstraints.Translate(err))
	}
	return nil
}

// Notice implements Store.
//
// cross-org: platform-wide texts.
func (s gormStore) Notice(ctx context.Context, id uuid.UUID) (Notice, error) {
	return s.notice(ctx, id, false)
}

// LockNotice implements Store.
//
// cross-org: platform-wide texts.
func (s gormStore) LockNotice(ctx context.Context, id uuid.UUID) (Notice, error) {
	return s.notice(ctx, id, true)
}

// cross-org: platform-wide texts.
func (s gormStore) notice(ctx context.Context, id uuid.UUID, lock bool) (Notice, error) {
	q := db.Conn(ctx, s.orm)
	if lock {
		q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	var n Notice
	if err := q.Where("id = ?", id).Take(&n).Error; err != nil {
		if db.IsNotFound(err) {
			return Notice{}, ErrNotFound
		}
		return Notice{}, fmt.Errorf("consent: notice: %w", err)
	}
	return noticeUTC(n), nil
}

// LockCurrent implements Store.
//
// cross-org: platform-wide texts.
func (s gormStore) LockCurrent(ctx context.Context, key NoticeKey) (Notice, bool, error) {
	var out []Notice
	err := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
		Where("kind = ? AND coalesce(purpose_code, '') = ? AND audience = ? AND locale = ?",
			string(key.Kind), string(key.purpose()), string(key.Audience), key.Locale).
		Where("published_at IS NOT NULL AND retired_at IS NULL").Limit(1).Find(&out).Error
	if err != nil {
		return Notice{}, false, fmt.Errorf("consent: lock current notice: %w", err)
	}
	if len(out) == 0 {
		return Notice{}, false, nil
	}
	return noticeUTC(out[0]), true, nil
}

// CurrentNotices implements Store.
//
// cross-org: platform-wide texts.
func (s gormStore) CurrentNotices(ctx context.Context, q CurrentQuery) ([]Notice, error) {
	tx := db.Conn(ctx, s.orm).Where("published_at IS NOT NULL AND retired_at IS NULL")
	if q.Kind != nil {
		tx = tx.Where("kind = ?", string(*q.Kind))
	}
	if q.Purpose != nil {
		tx = tx.Where("purpose_code = ?", string(*q.Purpose))
	}
	if len(q.Audiences) > 0 {
		aud := make([]string, len(q.Audiences))
		for i, a := range q.Audiences {
			aud[i] = string(a)
		}
		tx = tx.Where("audience IN ?", aud)
	}
	if len(q.Locales) > 0 {
		tx = tx.Where("locale IN ?", q.Locales)
	}
	var out []Notice
	if err := tx.Order("kind, coalesce(purpose_code, ''), audience, locale").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("consent: current notices: %w", err)
	}
	for i := range out {
		out[i] = noticeUTC(out[i])
	}
	return out, nil
}

// Publish implements Store.
//
// cross-org: platform-wide texts.
func (s gormStore) Publish(ctx context.Context, id uuid.UUID, at time.Time) error {
	res := db.Conn(ctx, s.orm).Model(&Notice{}).Where("id = ? AND published_at IS NULL", id).Update("published_at", at)
	if res.Error != nil {
		return fmt.Errorf("consent: publish notice: %w", noticeConstraints.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNoticeState
	}
	return nil
}

// Retire implements Store: the one change notice_versions_guard allows on
// a published row.
//
// cross-org: platform-wide texts.
func (s gormStore) Retire(ctx context.Context, id uuid.UUID, at time.Time) error {
	res := db.Conn(ctx, s.orm).Model(&Notice{}).
		Where("id = ? AND published_at IS NOT NULL AND retired_at IS NULL", id).Update("retired_at", at)
	if res.Error != nil {
		return fmt.Errorf("consent: retire notice: %w", noticeConstraints.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNoticeState
	}
	return nil
}

// InsertConsent implements Store (the only write of the append-only
// ledger; app_rw has no UPDATE or DELETE on consents).
//
// cross-org: personal ledger addressed by user.
func (s gormStore) InsertConsent(ctx context.Context, c Consent) error {
	err := db.Conn(ctx, s.orm).Exec(`INSERT INTO consents (id, user_id, purpose_code, action, notice_version_id,
		text_sha256, scope_type, scope_id, channel, ip, user_agent, occurred_at)
		VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, CAST( ? AS inet), ?, ?)`,
		c.ID, c.UserID, string(c.PurposeCode), string(c.Action), c.NoticeVersionID, c.TextSHA256,
		string(c.ScopeType), c.ScopeID, string(c.Channel), nullable(c.IP), nullable(c.UserAgent), c.OccurredAt).Error
	if err != nil {
		return fmt.Errorf("consent: insert consent: %w", db.Translate(err))
	}
	return nil
}

// Latest implements Store (index consents_current_idx).
//
// cross-org: personal ledger addressed by user.
func (s gormStore) Latest(ctx context.Context, userID uuid.UUID, purpose Purpose, scopeID *uuid.UUID) (Consent, bool, error) {
	q := db.Conn(ctx, s.orm).Table("consents").Select(consentCols).
		Where("user_id = ? AND purpose_code = ?", userID, string(purpose))
	if scopeID == nil {
		q = q.Where("scope_id IS NULL")
	} else {
		q = q.Where("scope_id = ?", *scopeID)
	}
	var rows []consentRow
	if err := q.Order("occurred_at DESC, id DESC").Limit(1).Find(&rows).Error; err != nil {
		return Consent{}, false, fmt.Errorf("consent: latest: %w", err)
	}
	if len(rows) == 0 {
		return Consent{}, false, nil
	}
	return toConsent(rows[0]), true, nil
}

// CurrentOf implements Store over the consent_current view.
//
// cross-org: personal ledger addressed by user.
func (s gormStore) CurrentOf(ctx context.Context, userID uuid.UUID) ([]Consent, error) {
	var rows []consentRow
	err := db.Conn(ctx, s.orm).Table("consent_current").Select(consentCols).Where("user_id = ?", userID).
		Order("purpose_code, scope_id NULLS FIRST").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("consent: current of user: %w", err)
	}
	return consents(rows), nil
}

// grantedBatch bounds the IN list of one GrantedAmong query.
const grantedBatch = 1000

// GrantedAmong implements Store over the consent_current view: the
// user_id/purpose_code/scope_id predicates are pushed into the DISTINCT ON
// (they are its keys), action is filtered after it.
//
// cross-org: personal ledger addressed by user.
func (s gormStore) GrantedAmong(ctx context.Context, purpose Purpose, userIDs []uuid.UUID) ([]uuid.UUID, error) {
	ids := sortedIDs(userIDs)
	out := make([]uuid.UUID, 0, len(ids))
	for start := 0; start < len(ids); start += grantedBatch {
		batch := ids[start:min(start+grantedBatch, len(ids))]
		var got []uuid.UUID
		err := db.Conn(ctx, s.orm).Table("consent_current").
			Where("purpose_code = ? AND scope_id IS NULL AND action = 'granted' AND user_id IN ?", string(purpose), batch).
			Order("user_id").Pluck("user_id", &got).Error
		if err != nil {
			return nil, fmt.Errorf("consent: granted among: %w", err)
		}
		out = append(out, got...)
	}
	return out, nil
}

// ListConsents implements Store (keyset over occurred_at DESC, id DESC).
//
// cross-org: personal ledger addressed by user.
func (s gormStore) ListConsents(ctx context.Context, q ListQuery) ([]Consent, error) {
	tx := db.Conn(ctx, s.orm).Table("consents").Select(consentCols).Where("user_id = ?", q.UserID)
	if q.Purpose != nil {
		tx = tx.Where("purpose_code = ?", string(*q.Purpose))
	}
	if q.After != nil {
		tx = tx.Where("(occurred_at, id) < ( ?, ? )", q.After.At, q.After.ID)
	}
	if q.Limit > 0 {
		tx = tx.Limit(q.Limit)
	}
	var rows []consentRow
	if err := tx.Order("occurred_at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("consent: list consents: %w", err)
	}
	return consents(rows), nil
}

// ConsentByID implements Store.
//
// cross-org: personal ledger addressed by ID.
func (s gormStore) ConsentByID(ctx context.Context, id uuid.UUID) (Consent, error) {
	var rows []consentRow
	if err := db.Conn(ctx, s.orm).Table("consents").Select(consentCols).Where("id = ?", id).Find(&rows).Error; err != nil {
		return Consent{}, fmt.Errorf("consent: consent: %w", err)
	}
	if len(rows) == 0 {
		return Consent{}, ErrNotFound
	}
	return toConsent(rows[0]), nil
}

// AllOf implements Store.
//
// cross-org: personal ledger addressed by user.
func (s gormStore) AllOf(ctx context.Context, userID uuid.UUID) ([]Consent, error) {
	var rows []consentRow
	err := db.Conn(ctx, s.orm).Table("consents").Select(consentCols).Where("user_id = ?", userID).
		Order("occurred_at, id").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("consent: consents of user: %w", err)
	}
	return consents(rows), nil
}

// InsertSubmission implements Store.
//
// cross-org: delivery rows of the personal ledger.
func (s gormStore) InsertSubmission(ctx context.Context, n Submission) error {
	if err := db.Conn(ctx, s.orm).Create(&n).Error; err != nil {
		return fmt.Errorf("consent: insert iys submission: %w", db.Translate(err))
	}
	return nil
}

// SubmissionOf implements Store.
//
// cross-org: delivery rows of the personal ledger.
func (s gormStore) SubmissionOf(ctx context.Context, consentID uuid.UUID) (Submission, error) {
	return s.submission(ctx, "consent_id = ?", consentID, false)
}

// LockSubmission implements Store.
//
// cross-org: delivery rows of the personal ledger.
func (s gormStore) LockSubmission(ctx context.Context, id uuid.UUID) (Submission, error) {
	return s.submission(ctx, "id = ?", id, true)
}

// cross-org: delivery rows of the personal ledger.
func (s gormStore) submission(ctx context.Context, cond string, id uuid.UUID, lock bool) (Submission, error) {
	q := db.Conn(ctx, s.orm)
	if lock {
		q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	var n Submission
	if err := q.Where(cond, id).Take(&n).Error; err != nil {
		if db.IsNotFound(err) {
			return Submission{}, ErrNotFound
		}
		return Submission{}, fmt.Errorf("consent: iys submission: %w", err)
	}
	n.CreatedAt, n.UpdatedAt, n.SentAt = n.CreatedAt.UTC(), n.UpdatedAt.UTC(), utcPtr(n.SentAt)
	return n, nil
}

// UpdateSubmission implements Store.
//
// cross-org: delivery rows of the personal ledger.
func (s gormStore) UpdateSubmission(ctx context.Context, n Submission) error {
	res := db.Conn(ctx, s.orm).Model(&Submission{}).Where("id = ?", n.ID).Updates(map[string]any{
		"status": string(n.Status), "iys_transaction_id": n.IYSTransactionID, "attempts": n.Attempts,
		"last_error": n.LastError, "sent_at": n.SentAt,
	})
	if res.Error != nil {
		return fmt.Errorf("consent: update iys submission: %w", db.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// DueSubmissions implements Store (index iys_submissions_pending_idx).
//
// cross-org: platform job over every user.
func (s gormStore) DueSubmissions(ctx context.Context, before time.Time, after uuid.UUID, limit int) ([]Submission, error) {
	var out []Submission
	err := db.Conn(ctx, s.orm).
		Where("status IN ('pending','failed') AND updated_at < ? AND id > ?", before, after).
		Order("id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("consent: due iys submissions: %w", err)
	}
	return out, nil
}

// PurgeProof implements Store.
//
// cross-org: retention spans every user.
func (s gormStore) PurgeProof(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT consents_purge( ?, ?, ?)`, cutoff, max(limit, 1), dryRun).Scan(&n).Error; err != nil {
		return 0, fmt.Errorf("consent: purge proof: %w", db.Translate(err))
	}
	return int(n), nil
}
