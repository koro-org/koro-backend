package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/civil"
	"koro/internal/platform/db"
)

// chainLockKey is the transaction advisory lock of the chain job ("koroaudt").
const chainLockKey int64 = 0x6b6f726f61756474

// eventCols are the selected columns; ip is read as host text so the hash
// sees the same canonical form as the in-memory store.
const eventCols = `id, seq, occurred_at, actor_kind, actor_user_id, actor_org_id, system_job, action, outcome,
	org_id, resource_type, resource_id, reason, host(ip) AS ip, user_agent, request_id, metadata,
	chain_seq, prev_hash, row_hash, chained_at`

// eventRow scans one audit_events row.
type eventRow struct {
	ID           uuid.UUID       `gorm:"column:id"`
	Seq          int64           `gorm:"column:seq"`
	OccurredAt   time.Time       `gorm:"column:occurred_at"`
	ActorKind    string          `gorm:"column:actor_kind"`
	ActorUserID  *uuid.UUID      `gorm:"column:actor_user_id"`
	ActorOrgID   *uuid.UUID      `gorm:"column:actor_org_id"`
	SystemJob    *string         `gorm:"column:system_job"`
	Action       string          `gorm:"column:action"`
	Outcome      string          `gorm:"column:outcome"`
	OrgID        *uuid.UUID      `gorm:"column:org_id"`
	ResourceType *string         `gorm:"column:resource_type"`
	ResourceID   *uuid.UUID      `gorm:"column:resource_id"`
	Reason       *string         `gorm:"column:reason"`
	IP           *string         `gorm:"column:ip"`
	UserAgent    *string         `gorm:"column:user_agent"`
	RequestID    *string         `gorm:"column:request_id"`
	Metadata     json.RawMessage `gorm:"column:metadata"`
	ChainSeq     *int64          `gorm:"column:chain_seq"`
	PrevHash     []byte          `gorm:"column:prev_hash"`
	RowHash      []byte          `gorm:"column:row_hash"`
	ChainedAt    *time.Time      `gorm:"column:chained_at"`
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func toRecord(r eventRow) Record {
	out := Record{
		ID: r.ID, Seq: r.Seq, OccurredAt: r.OccurredAt.UTC(), ActorKind: r.ActorKind, ActorUserID: r.ActorUserID,
		ActorOrgID: r.ActorOrgID, SystemJob: deref(r.SystemJob), Action: r.Action, Outcome: Outcome(r.Outcome),
		OrgID: r.OrgID, ResourceType: deref(r.ResourceType), ResourceID: r.ResourceID, Reason: deref(r.Reason),
		IP: canonicalIP(deref(r.IP)), UserAgent: deref(r.UserAgent), RequestID: deref(r.RequestID),
		Metadata: r.Metadata, ChainSeq: r.ChainSeq, PrevHash: r.PrevHash, RowHash: r.RowHash,
	}
	if r.ChainedAt != nil {
		at := r.ChainedAt.UTC()
		out.ChainedAt = &at
	}
	return out
}

func records(rows []eventRow) []Record {
	out := make([]Record, len(rows))
	for i, r := range rows {
		out[i] = toRecord(r)
	}
	return out
}

// rootRow scans one audit_daily_roots row.
type rootRow struct {
	Day          civil.Date `gorm:"column:day"`
	LastChainSeq int64      `gorm:"column:last_chain_seq"`
	RootHash     []byte     `gorm:"column:root_hash"`
	Signature    []byte     `gorm:"column:signature"`
	KeyID        string     `gorm:"column:key_id"`
	ExternalRef  string     `gorm:"column:external_ref"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
}

func toRoot(r rootRow) DailyRoot {
	return DailyRoot{Day: r.Day, LastChainSeq: r.LastChainSeq, RootHash: r.RootHash, Signature: r.Signature,
		KeyID: r.KeyID, ExternalRef: r.ExternalRef, CreatedAt: r.CreatedAt.UTC()}
}

const rootCols = `day, last_chain_seq, root_hash, signature, key_id, external_ref, created_at`

// NewGormStore returns the PostgreSQL store (connects as app_rw).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// errNoTx: BeginChain outside a transaction would leave the role switched
// on a pooled connection.
var errNoTx = errors.New("audit: BeginChain needs a transaction")

// Insert implements Store.
//
// cross-org: append-only log of every org; the row carries org_id as data.
func (s gormStore) Insert(ctx context.Context, r Record) error {
	meta := r.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	err := db.Conn(ctx, s.orm).Exec(`INSERT INTO audit_events (id, occurred_at, actor_kind, actor_user_id, actor_org_id,
		system_job, action, outcome, org_id, resource_type, resource_id, reason, ip, user_agent, request_id, metadata)
		VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CAST( ? AS inet), ?, ?, CAST( ? AS jsonb))`,
		r.ID, r.OccurredAt, r.ActorKind, r.ActorUserID, r.ActorOrgID, nullable(r.SystemJob), r.Action, string(r.Outcome),
		r.OrgID, nullable(r.ResourceType), r.ResourceID, nullable(r.Reason), nullable(r.IP), nullable(r.UserAgent),
		nullable(r.RequestID), string(meta)).Error
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// BeginChain implements Store: the advisory lock and the last root are
// taken as app_rw, then the transaction switches to audit_chainer (which
// may read audit_events and update its chain columns, nothing else).
//
// cross-org: the chain spans every org.
func (s gormStore) BeginChain(ctx context.Context) (Link, error) {
	if !db.InTx(ctx) {
		return Link{}, errNoTx
	}
	conn := db.Conn(ctx, s.orm)
	if err := conn.Exec(`SELECT pg_advisory_xact_lock( ? )`, chainLockKey).Error; err != nil {
		return Link{}, fmt.Errorf("audit: chain lock: %w", err)
	}
	var root []rootRow
	if err := conn.Raw(`SELECT ` + rootCols + ` FROM audit_daily_roots ORDER BY day DESC LIMIT 1`).Scan(&root).Error; err != nil {
		return Link{}, fmt.Errorf("audit: last root: %w", err)
	}
	if err := conn.Exec(`SET LOCAL ROLE audit_chainer`).Error; err != nil {
		return Link{}, fmt.Errorf("audit: chainer role: %w", err)
	}
	head, err := s.head(ctx, `chain_seq IS NOT NULL`)
	if err != nil {
		return Link{}, err
	}
	if len(root) == 1 && root[0].LastChainSeq > head.Seq {
		head = Link{Seq: root[0].LastChainSeq, Hash: root[0].RootHash}
	}
	return head, nil
}

// head returns the newest chained row matching where.
//
// cross-org: the chain spans every org.
func (s gormStore) head(ctx context.Context, where string, args ...any) (Link, error) {
	var rows []struct {
		ChainSeq int64  `gorm:"column:chain_seq"`
		RowHash  []byte `gorm:"column:row_hash"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT chain_seq, row_hash FROM audit_events WHERE `+where+
		` ORDER BY chain_seq DESC LIMIT 1`, args...).Scan(&rows).Error
	if err != nil {
		return Link{}, fmt.Errorf("audit: chain head: %w", err)
	}
	if len(rows) == 0 {
		return Link{Hash: zeroHash}, nil
	}
	return Link{Seq: rows[0].ChainSeq, Hash: rows[0].RowHash}, nil
}

// Unchained implements Store.
//
// cross-org: the chain spans every org.
func (s gormStore) Unchained(ctx context.Context, limit int) ([]Record, error) {
	var rows []eventRow
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+eventCols+` FROM audit_events WHERE chain_seq IS NULL ORDER BY seq LIMIT ?`, limit).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("audit: unchained rows: %w", err)
	}
	return records(rows), nil
}

// SetChain implements Store.
//
// cross-org: the chain spans every org.
func (s gormStore) SetChain(ctx context.Context, id uuid.UUID, seq int64, prev, hash []byte, at time.Time) error {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE audit_events SET chain_seq = ?, prev_hash = ?, row_hash = ?, chained_at = ?
		WHERE id = ? AND chain_seq IS NULL`, seq, prev, hash, at, id)
	if res.Error != nil {
		return fmt.Errorf("audit: set chain: %w", res.Error)
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("audit: set chain: %w", errNoSuchRow)
	}
	return nil
}

// Chained implements Store.
//
// cross-org: the chain spans every org.
func (s gormStore) Chained(ctx context.Context, after int64, limit int) ([]Record, error) {
	var rows []eventRow
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+eventCols+` FROM audit_events WHERE chain_seq > ? ORDER BY chain_seq LIMIT ?`,
		after, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("audit: chained rows: %w", err)
	}
	return records(rows), nil
}

// HeadAt implements Store.
//
// cross-org: the chain spans every org.
func (s gormStore) HeadAt(ctx context.Context, before time.Time) (Link, error) {
	return s.head(ctx, `chain_seq IS NOT NULL AND chained_at < ?`, before)
}

// OldestUnchained implements Store.
//
// cross-org: the chain spans every org.
func (s gormStore) OldestUnchained(ctx context.Context) (time.Time, bool, error) {
	var oldest *time.Time
	err := db.Conn(ctx, s.orm).Raw(`SELECT min(occurred_at) FROM audit_events WHERE chain_seq IS NULL`).Scan(&oldest).Error
	if err != nil {
		return time.Time{}, false, fmt.Errorf("audit: oldest unchained: %w", err)
	}
	if oldest == nil {
		return time.Time{}, false, nil
	}
	return oldest.UTC(), true, nil
}

// InsertRoot implements Store.
//
// cross-org: daily roots have no org.
func (s gormStore) InsertRoot(ctx context.Context, r DailyRoot) error {
	err := db.Conn(ctx, s.orm).Exec(`INSERT INTO audit_daily_roots (day, last_chain_seq, root_hash, signature, key_id, external_ref)
		VALUES ( ?, ?, ?, ?, ?, ?)`, r.Day, r.LastChainSeq, r.RootHash, r.Signature, r.KeyID, r.ExternalRef).Error
	switch {
	case db.IsUnique(err):
		return ErrRootExists.Wrap(err)
	case err != nil:
		return fmt.Errorf("audit: insert root: %w", err)
	}
	return nil
}

// roots runs a daily root query.
//
// cross-org: daily roots have no org.
func (s gormStore) roots(ctx context.Context, query string, args ...any) ([]DailyRoot, error) {
	var rows []rootRow
	if err := db.Conn(ctx, s.orm).Raw(`SELECT `+rootCols+` FROM audit_daily_roots `+query, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("audit: daily roots: %w", err)
	}
	out := make([]DailyRoot, len(rows))
	for i, r := range rows {
		out[i] = toRoot(r)
	}
	return out, nil
}

func first(rs []DailyRoot, err error) (DailyRoot, bool, error) {
	if err != nil || len(rs) == 0 {
		return DailyRoot{}, false, err
	}
	return rs[0], true, nil
}

// LastRoot implements Store.
//
// cross-org: daily roots have no org.
func (s gormStore) LastRoot(ctx context.Context) (DailyRoot, bool, error) {
	return first(s.roots(ctx, `ORDER BY day DESC LIMIT 1`))
}

// RootBefore implements Store.
//
// cross-org: daily roots have no org.
func (s gormStore) RootBefore(ctx context.Context, day civil.Date) (DailyRoot, bool, error) {
	return first(s.roots(ctx, `WHERE day < ? ORDER BY day DESC LIMIT 1`, day))
}

// Roots implements Store.
//
// cross-org: daily roots have no org.
func (s gormStore) Roots(ctx context.Context, from, to civil.Date) ([]DailyRoot, error) {
	return s.roots(ctx, `WHERE day BETWEEN ? AND ? ORDER BY day`, from, to)
}

// keyset applies the keyset position and the order of every list.
func keyset(q *gorm.DB, after *After, limit int) *gorm.DB {
	if after != nil {
		q = q.Where(`(occurred_at, id) < ( ?, ? )`, after.At, after.ID)
	}
	return q.Order("occurred_at DESC, id DESC").Limit(limit)
}

// find runs a list query built (and scoped) by the caller.
//
// cross-org: the callers apply the scope (ListOrg: ScopeOrg).
func (s gormStore) find(q *gorm.DB) ([]Record, error) {
	var rows []eventRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	return records(rows), nil
}

// ListOwn implements Store.
//
// cross-org: the user's own events across every org (actor or subject).
func (s gormStore) ListOwn(ctx context.Context, user uuid.UUID, after *After, limit int) ([]Record, error) {
	sub, err := json.Marshal(map[string]any{MetaBreakGlass: true, MetaSubjectUserID: user.String()})
	if err != nil {
		return nil, fmt.Errorf("audit: list own: %w", err)
	}
	q := db.Conn(ctx, s.orm).Table("audit_events").Select(eventCols).
		Where(`(actor_user_id = ? AND (action LIKE 'identity.%' OR action LIKE 'consent.%')) OR metadata @> CAST( ? AS jsonb)`,
			user, string(sub))
	return s.find(keyset(q, after, limit))
}

// ListOrg implements Store.
func (s gormStore) ListOrg(ctx context.Context, orgID uuid.UUID, f Filter, after *After, limit int) ([]Record, error) {
	f.OrgID = nil
	q := db.Conn(ctx, s.orm).Table("audit_events").Select(eventCols).Scopes(db.ScopeOrg(orgID))
	return s.find(keyset(applyFilter(q, f), after, limit))
}

// List implements Store.
//
// cross-org: adminListAuditEvents searches every org (dpo, platform_admin).
func (s gormStore) List(ctx context.Context, f Filter, after *After, limit int) ([]Record, error) {
	q := db.Conn(ctx, s.orm).Table("audit_events").Select(eventCols)
	return s.find(keyset(applyFilter(q, f), after, limit))
}

func applyFilter(q *gorm.DB, f Filter) *gorm.DB {
	if !f.From.IsZero() {
		q = q.Where("occurred_at >= ?", f.From)
	}
	if !f.To.IsZero() {
		q = q.Where("occurred_at < ?", f.To)
	}
	if f.ActorUserID != nil {
		q = q.Where("actor_user_id = ?", *f.ActorUserID)
	}
	if f.OrgID != nil {
		q = q.Where("org_id = ?", *f.OrgID)
	}
	if f.ResourceType != "" {
		q = q.Where("resource_type = ?", f.ResourceType)
	}
	if f.ResourceID != nil {
		q = q.Where("resource_id = ?", *f.ResourceID)
	}
	if f.Action != "" {
		q = q.Where("action = ?", f.Action)
	}
	if f.Outcome != "" {
		q = q.Where("outcome = ?", string(f.Outcome))
	}
	if f.BreakGlass != nil {
		if *f.BreakGlass {
			q = q.Where(`metadata @> '{"break_glass": true}'`)
		} else {
			q = q.Where(`NOT metadata @> '{"break_glass": true}'`)
		}
	}
	if f.HideSensitive {
		q = q.Where(`NOT metadata @> '{"sensitive": true}'`)
	}
	return q
}
