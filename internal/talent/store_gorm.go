package talent

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

// gormStore is the PostgreSQL store of the talent schema. Its pool is the
// application pool (app_rw); every statement runs after Enter switched the
// transaction to app_talent, the only role that reaches the schema
// (02 §15, 05 §15.4).
type gormStore struct{ orm *gorm.DB }

// NewGormStore returns the GORM store over the app_rw pool orm.
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

// roleKey marks a context whose transaction runs as app_talent (true) or
// was switched back to the session role by AsApp (false).
type roleKey struct{}

func withRole(ctx context.Context, on bool) context.Context {
	return context.WithValue(ctx, roleKey{}, on)
}

func inRole(ctx context.Context) bool { on, _ := ctx.Value(roleKey{}).(bool); return on }

// Enter implements Store: SET LOCAL ROLE app_talent ends with the
// transaction, so the pooled connection is app_rw again afterwards.
//
// cross-org: no org data; switches the role of the caller's transaction.
func (s gormStore) Enter(ctx context.Context) (context.Context, error) {
	if !db.InTx(ctx) {
		return ctx, errNoTalentTx
	}
	if err := db.Conn(ctx, s.orm).Exec(`SET LOCAL ROLE app_talent`).Error; err != nil {
		return ctx, fmt.Errorf("talent: set role app_talent: %w", err)
	}
	return withRole(ctx, true), nil
}

// AsApp implements Store.
//
// cross-org: no org data; switches the role of the caller's transaction.
func (s gormStore) AsApp(ctx context.Context, fn func(context.Context) error) error {
	c, err := s.conn(ctx)
	if err != nil {
		return err
	}
	if err := c.Exec(`SET LOCAL ROLE NONE`).Error; err != nil {
		return fmt.Errorf("talent: reset role: %w", err)
	}
	if err := fn(withRole(ctx, false)); err != nil {
		return err
	}
	if err := db.Conn(ctx, s.orm).Exec(`SET LOCAL ROLE app_talent`).Error; err != nil {
		return fmt.Errorf("talent: set role app_talent: %w", err)
	}
	return nil
}

// Leave implements Store.
//
// cross-org: no org data; switches the role of the caller's transaction.
func (s gormStore) Leave(ctx context.Context) (context.Context, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return ctx, err
	}
	if err := c.Exec(`SET LOCAL ROLE NONE`).Error; err != nil {
		return ctx, fmt.Errorf("talent: reset role: %w", err)
	}
	return withRole(ctx, false), nil
}

// conn returns the transaction of ctx once Enter switched it to app_talent.
//
// cross-org: no statement; the transaction of every talent method.
func (s gormStore) conn(ctx context.Context) (*gorm.DB, error) {
	if !db.InTx(ctx) || !inRole(ctx) {
		return nil, errNoTalentTx
	}
	return db.Conn(ctx, s.orm), nil
}

func wrap(op string, err error) error { return fmt.Errorf("talent: %s: %w", op, db.Translate(err)) }

// ---------------------------------------------------------------- signals

type signalRow struct {
	Code                  string `gorm:"column:code"`
	Category              string `gorm:"column:category"`
	BasePointsCP          int    `gorm:"column:base_points_cp"`
	DailyCapCP            *int   `gorm:"column:daily_cap_cp"`
	WeeklyCapCP           *int   `gorm:"column:weekly_cap_cp"`
	HalfLifeDays          *int   `gorm:"column:half_life_days"`
	ExcludedWhenSensitive bool   `gorm:"column:excluded_when_sensitive"`
	Enabled               bool   `gorm:"column:enabled"`
}

const signalCols = `code, category, base_points_cp, daily_cap_cp, weekly_cap_cp, half_life_days, excluded_when_sensitive, enabled`

// def converts the row.
//
// cross-org: no statement.
func (r signalRow) def() SignalDef { return SignalDef(r) }

// SignalDef implements Store.
//
// cross-org: the signal catalogue is platform data.
func (s gormStore) SignalDef(ctx context.Context, code string) (SignalDef, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return SignalDef{}, err
	}
	var rows []signalRow
	if err := c.Raw(`SELECT `+signalCols+` FROM talent.signal_defs WHERE code = ?`, code).Scan(&rows).Error; err != nil {
		return SignalDef{}, wrap("signal def", err)
	}
	if len(rows) == 0 {
		return SignalDef{}, ErrNotFound
	}
	return rows[0].def(), nil
}

// SignalDefs implements Store.
//
// cross-org: the signal catalogue is platform data.
func (s gormStore) SignalDefs(ctx context.Context) ([]SignalDef, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	var rows []signalRow
	if err := c.Raw(`SELECT ` + signalCols + ` FROM talent.signal_defs ORDER BY code`).Scan(&rows).Error; err != nil {
		return nil, wrap("signal defs", err)
	}
	out := make([]SignalDef, len(rows))
	for i, r := range rows {
		out[i] = r.def()
	}
	return out, nil
}

// UpdateSignalDef implements Store. updated_at is the touch trigger's
// (02 §0.1: a [ts] column is never written by the application).
//
// cross-org: the signal catalogue is platform data.
func (s gormStore) UpdateSignalDef(ctx context.Context, d SignalDef, by uuid.UUID) (SignalDef, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return SignalDef{}, err
	}
	var rows []signalRow
	err = c.Raw(`UPDATE talent.signal_defs SET base_points_cp = ?, daily_cap_cp = ?, weekly_cap_cp = ?, half_life_days = ?,
		enabled = ?, updated_by = ? WHERE code = ? RETURNING `+signalCols,
		d.BasePointsCP, d.DailyCapCP, d.WeeklyCapCP, d.HalfLifeDays, d.Enabled, by, d.Code).Scan(&rows).Error
	if err != nil {
		return SignalDef{}, wrap("update signal def", err)
	}
	if len(rows) == 0 {
		return SignalDef{}, ErrNotFound
	}
	return rows[0].def(), nil
}

// ---------------------------------------------------------------- config

type configRow struct {
	ID            uuid.UUID `gorm:"column:id"`
	EffectiveFrom time.Time `gorm:"column:effective_from"`
	Params        string    `gorm:"column:params"`
	Notes         *string   `gorm:"column:notes"`
	CreatedBy     uuid.UUID `gorm:"column:created_by"`
	CreatedAt     time.Time `gorm:"column:created_at"`
}

const configCols = `id, effective_from, params::text AS params, notes, created_by, created_at`

// version converts the row.
//
// cross-org: no statement.
func (r configRow) version() ConfigVersion {
	v := ConfigVersion{ID: r.ID, EffectiveFrom: r.EffectiveFrom, Params: json.RawMessage(r.Params), CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt}
	if r.Notes != nil {
		v.Notes = *r.Notes
	}
	return v
}

// CurrentConfig implements Store.
//
// cross-org: the score configuration is platform data.
func (s gormStore) CurrentConfig(ctx context.Context, at time.Time) (ConfigVersion, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return ConfigVersion{}, err
	}
	var rows []configRow
	err = c.Raw(`SELECT `+configCols+` FROM talent.config_versions WHERE effective_from <= ? ORDER BY effective_from DESC LIMIT 1`, at).
		Scan(&rows).Error
	if err != nil {
		return ConfigVersion{}, wrap("current config", err)
	}
	if len(rows) == 0 {
		return ConfigVersion{}, ErrNotFound
	}
	return rows[0].version(), nil
}

// InsertConfigVersion implements Store.
//
// cross-org: the score configuration is platform data.
func (s gormStore) InsertConfigVersion(ctx context.Context, v ConfigVersion) (ConfigVersion, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return ConfigVersion{}, err
	}
	params := string(v.Params)
	if params == "" {
		params = "{}"
	}
	var rows []configRow
	err = c.Raw(`INSERT INTO talent.config_versions (id, effective_from, params, notes, created_by)
		VALUES ( ?, ?, CAST( ? AS jsonb), ?, ?) RETURNING `+configCols,
		v.ID, v.EffectiveFrom, params, nullArg(v.Notes), v.CreatedBy).Scan(&rows).Error
	if err != nil {
		return ConfigVersion{}, wrap("insert config version", err)
	}
	return rows[0].version(), nil
}

// ---------------------------------------------------------------- counters

// LockCounter implements Store: one upsert creates the row or locks the
// existing one (DO UPDATE takes the row lock even without a change) and
// returns the committed value after a concurrent writer finished.
//
// cross-org: the subject's own counter.
func (s gormStore) LockCounter(ctx context.Context, userID uuid.UUID, day civil.Date, signal string) (int, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return 0, err
	}
	var applied []int
	err = c.Raw(`INSERT INTO talent.daily_counters (user_id, day, signal_code, applied_cp) VALUES ( ?, ?, ?, 0)
		ON CONFLICT (user_id, day, signal_code) DO UPDATE SET applied_cp = talent.daily_counters.applied_cp
		RETURNING applied_cp`, userID, day, signal).Scan(&applied).Error
	if err != nil {
		return 0, wrap("lock counter", err)
	}
	return applied[0], nil
}

// AppliedBetween implements Store.
//
// cross-org: the subject's own counters.
func (s gormStore) AppliedBetween(ctx context.Context, userID uuid.UUID, signal string, from, to civil.Date) (int, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	err = c.Raw(`SELECT COALESCE(sum(applied_cp), 0) FROM talent.daily_counters
		WHERE user_id = ? AND signal_code = ? AND day BETWEEN ? AND ?`, userID, signal, from, to).Scan(&n).Error
	if err != nil {
		return 0, wrap("applied between", err)
	}
	return n, nil
}

// AddCounter implements Store.
//
// cross-org: the subject's own counter.
func (s gormStore) AddCounter(ctx context.Context, userID uuid.UUID, day civil.Date, signal string, delta int) error {
	c, err := s.conn(ctx)
	if err != nil {
		return err
	}
	if err := c.Exec(`UPDATE talent.daily_counters SET applied_cp = applied_cp + ? WHERE user_id = ? AND day = ? AND signal_code = ?`,
		delta, userID, day, signal).Error; err != nil {
		return wrap("add counter", err)
	}
	return nil
}

// ---------------------------------------------------------------- ledger

type pointRow struct {
	ID              uuid.UUID  `gorm:"column:id"`
	UserID          uuid.UUID  `gorm:"column:user_id"`
	SignalCode      string     `gorm:"column:signal_code"`
	SourceType      string     `gorm:"column:source_type"`
	SourceID        uuid.UUID  `gorm:"column:source_id"`
	OccurredAt      time.Time  `gorm:"column:occurred_at"`
	RawPointsCP     int        `gorm:"column:raw_points_cp"`
	PointsCP        int        `gorm:"column:points_cp"`
	Capped          bool       `gorm:"column:capped"`
	CapReason       *string    `gorm:"column:cap_reason"`
	ReasonCode      string     `gorm:"column:reason_code"`
	ConfigVersionID uuid.UUID  `gorm:"column:config_version_id"`
	ReversalOf      *uuid.UUID `gorm:"column:reversal_of"`
	SourceEventID   uuid.UUID  `gorm:"column:source_event_id"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
}

const pointCols = `id, user_id, signal_code, source_type, source_id, occurred_at, raw_points_cp, points_cp, capped, cap_reason,
	reason_code, config_version_id, reversal_of, source_event_id, created_at`

// point converts the row.
//
// cross-org: no statement.
func (r pointRow) point() PointEvent {
	p := PointEvent{ID: r.ID, UserID: r.UserID, Signal: r.SignalCode, SourceType: r.SourceType, SourceID: r.SourceID,
		OccurredAt: r.OccurredAt, RawPointsCP: r.RawPointsCP, PointsCP: r.PointsCP, Capped: r.Capped, ReasonCode: r.ReasonCode,
		ConfigVersionID: r.ConfigVersionID, ReversalOf: r.ReversalOf, SourceEventID: r.SourceEventID, CreatedAt: r.CreatedAt}
	if r.CapReason != nil {
		p.CapReason = *r.CapReason
	}
	return p
}

// InsertPoint implements Store. ON CONFLICT DO NOTHING covers the dedupe
// key and the one-reversal-per-row index: a repeated or concurrent event
// inserts nothing (the second inserter waits for the first one's commit).
// created_at is the column default (read back by RETURNING).
//
// cross-org: the subject's own ledger.
func (s gormStore) InsertPoint(ctx context.Context, p PointEvent) (PointEvent, bool, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return PointEvent{}, false, err
	}
	var rows []pointRow
	err = c.Raw(`INSERT INTO talent.point_events (id, user_id, signal_code, source_type, source_id, occurred_at, raw_points_cp,
		points_cp, capped, cap_reason, reason_code, config_version_id, reversal_of, source_event_id)
		VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING RETURNING `+pointCols,
		p.ID, p.UserID, p.Signal, p.SourceType, p.SourceID, p.OccurredAt, p.RawPointsCP, p.PointsCP, p.Capped,
		nullArg(p.CapReason), p.ReasonCode, p.ConfigVersionID, p.ReversalOf, p.SourceEventID).Scan(&rows).Error
	if err != nil {
		return PointEvent{}, false, wrap("insert point", err)
	}
	if len(rows) == 0 {
		return PointEvent{}, false, nil
	}
	return rows[0].point(), true, nil
}

// FindPoint implements Store.
//
// cross-org: the subject's own ledger.
func (s gormStore) FindPoint(ctx context.Context, k SourceKey) (PointEvent, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return PointEvent{}, err
	}
	var rows []pointRow
	err = c.Raw(`SELECT `+pointCols+` FROM talent.point_events
		WHERE user_id = ? AND signal_code = ? AND source_type = ? AND source_id = ?`, k.UserID, k.Signal, k.SourceType, k.SourceID).
		Scan(&rows).Error
	if err != nil {
		return PointEvent{}, wrap("find point", err)
	}
	if len(rows) == 0 {
		return PointEvent{}, ErrNotFound
	}
	return rows[0].point(), nil
}

// CountCredited implements Store.
//
// cross-org: the subject's own ledger.
func (s gormStore) CountCredited(ctx context.Context, userID uuid.UUID, signal string) (int, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	err = c.Raw(`SELECT count(*) FROM talent.point_events p
		WHERE p.user_id = ? AND p.signal_code = ? AND p.points_cp > 0 AND p.reversal_of IS NULL
		  AND NOT EXISTS (SELECT 1 FROM talent.point_events r WHERE r.reversal_of = p.id)`, userID, signal).Scan(&n).Error
	if err != nil {
		return 0, wrap("count credited", err)
	}
	return n, nil
}

// Points implements Store.
//
// cross-org: the subject's own ledger.
func (s gormStore) Points(ctx context.Context, userID uuid.UUID) ([]PointEvent, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	var rows []pointRow
	if err := c.Raw(`SELECT `+pointCols+` FROM talent.point_events WHERE user_id = ? ORDER BY occurred_at, id`, userID).
		Scan(&rows).Error; err != nil {
		return nil, wrap("points", err)
	}
	out := make([]PointEvent, len(rows))
	for i, r := range rows {
		out[i] = r.point()
	}
	return out, nil
}

// GetPoint implements Store.
//
// cross-org: one ledger row by its ID (the caller authorises the auditor).
func (s gormStore) GetPoint(ctx context.Context, id uuid.UUID) (PointEvent, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return PointEvent{}, err
	}
	var rows []pointRow
	if err := c.Raw(`SELECT `+pointCols+` FROM talent.point_events WHERE id = ?`, id).Scan(&rows).Error; err != nil {
		return PointEvent{}, wrap("get point", err)
	}
	if len(rows) == 0 {
		return PointEvent{}, ErrNotFound
	}
	return rows[0].point(), nil
}

// PointsSince implements Store.
//
// cross-org: the nightly run's ledger read, keyed by the batch's user IDs.
func (s gormStore) PointsSince(ctx context.Context, userIDs []uuid.UUID, since time.Time) ([]PointEvent, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	c, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	var rows []pointRow
	err = c.Raw(`SELECT `+pointCols+` FROM talent.point_events WHERE user_id IN ? AND occurred_at > ?
		ORDER BY user_id, occurred_at, id`, userIDs, since).Scan(&rows).Error
	if err != nil {
		return nil, wrap("points since", err)
	}
	out := make([]PointEvent, len(rows))
	for i, r := range rows {
		out[i] = r.point()
	}
	return out, nil
}

// ---------------------------------------------------------------- neutralisations

type neutralRow struct {
	ID        uuid.UUID  `gorm:"column:id"`
	UserID    uuid.UUID  `gorm:"column:user_id"`
	Reason    string     `gorm:"column:reason"`
	DSRID     *uuid.UUID `gorm:"column:dsr_id"`
	Note      *string    `gorm:"column:note"`
	CreatedBy *uuid.UUID `gorm:"column:created_by"`
	CreatedAt time.Time  `gorm:"column:created_at"`
	LiftedAt  *time.Time `gorm:"column:lifted_at"`
	LiftedBy  *uuid.UUID `gorm:"column:lifted_by"`
}

const neutralCols = `id, user_id, reason, dsr_id, note, created_by, created_at, lifted_at, lifted_by`

// neutralization converts the row.
//
// cross-org: no statement.
func (r neutralRow) neutralization() Neutralization {
	n := Neutralization{ID: r.ID, UserID: r.UserID, Reason: NeutralizeReason(r.Reason), DSRID: r.DSRID, CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt, LiftedAt: r.LiftedAt, LiftedBy: r.LiftedBy}
	if r.Note != nil {
		n.Note = *r.Note
	}
	return n
}

func neutralizations(rows []neutralRow) []Neutralization {
	out := make([]Neutralization, len(rows))
	for i, r := range rows {
		out[i] = r.neutralization()
	}
	return out
}

// Neutralizations implements Store.
//
// cross-org: the subject's own neutralisations.
func (s gormStore) Neutralizations(ctx context.Context, userID uuid.UUID) ([]Neutralization, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	var rows []neutralRow
	if err := c.Raw(`SELECT `+neutralCols+` FROM talent.neutralizations WHERE user_id = ? ORDER BY created_at, id`, userID).
		Scan(&rows).Error; err != nil {
		return nil, wrap("neutralizations", err)
	}
	return neutralizations(rows), nil
}

// GetNeutralization implements Store.
//
// cross-org: one neutralisation by its ID (the caller authorises the subject).
func (s gormStore) GetNeutralization(ctx context.Context, id uuid.UUID) (Neutralization, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return Neutralization{}, err
	}
	var rows []neutralRow
	if err := c.Raw(`SELECT `+neutralCols+` FROM talent.neutralizations WHERE id = ?`, id).Scan(&rows).Error; err != nil {
		return Neutralization{}, wrap("get neutralization", err)
	}
	if len(rows) == 0 {
		return Neutralization{}, ErrNotFound
	}
	return rows[0].neutralization(), nil
}

// InsertNeutralization implements Store. ON CONFLICT DO NOTHING meets the
// partial unique index of active rows (one per user and reason).
//
// cross-org: the subject's own neutralisation.
func (s gormStore) InsertNeutralization(ctx context.Context, n Neutralization) (Neutralization, bool, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return Neutralization{}, false, err
	}
	var rows []neutralRow
	err = c.Raw(`INSERT INTO talent.neutralizations (id, user_id, reason, dsr_id, note, created_by)
		VALUES ( ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING RETURNING `+neutralCols,
		n.ID, n.UserID, string(n.Reason), n.DSRID, nullArg(n.Note), n.CreatedBy).Scan(&rows).Error
	if err != nil {
		return Neutralization{}, false, wrap("insert neutralization", err)
	}
	if len(rows) == 1 {
		return rows[0].neutralization(), true, nil
	}
	err = c.Raw(`SELECT `+neutralCols+` FROM talent.neutralizations WHERE user_id = ? AND reason = ? AND lifted_at IS NULL`,
		n.UserID, string(n.Reason)).Scan(&rows).Error
	if err != nil {
		return Neutralization{}, false, wrap("active neutralization", err)
	}
	if len(rows) == 0 {
		return Neutralization{}, false, fmt.Errorf("talent: neutralization of %s conflicted without an active row", n.UserID)
	}
	return rows[0].neutralization(), false, nil
}

// LiftNeutralization implements Store. lifted_at is a business time (the
// service clock), not a [ts] stamp.
//
// cross-org: one neutralisation by its ID (the caller authorises the subject).
func (s gormStore) LiftNeutralization(ctx context.Context, id uuid.UUID, by *uuid.UUID, at time.Time) (Neutralization, bool, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return Neutralization{}, false, err
	}
	var rows []neutralRow
	err = c.Raw(`UPDATE talent.neutralizations SET lifted_at = ?, lifted_by = ? WHERE id = ? AND lifted_at IS NULL RETURNING `+neutralCols,
		at, by, id).Scan(&rows).Error
	if err != nil {
		return Neutralization{}, false, wrap("lift neutralization", err)
	}
	if len(rows) == 1 {
		return rows[0].neutralization(), true, nil
	}
	n, err := s.GetNeutralization(ctx, id)
	return n, false, err
}

// ---------------------------------------------------------------- bands

// Bands implements Store.
//
// cross-org: ranking input of discovery, keyed by the result's user IDs.
func (s gormStore) Bands(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]Band, error) {
	out := map[uuid.UUID]Band{}
	if len(userIDs) == 0 {
		return out, nil
	}
	c, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		UserID uuid.UUID `gorm:"column:user_id"`
		Band   int16     `gorm:"column:band"`
	}
	err = c.Raw(`SELECT s.user_id, s.band FROM talent.scores s
		WHERE s.user_id IN ? AND s.band > 0 AND NOT s.neutralized
		  AND NOT EXISTS (SELECT 1 FROM talent.neutralizations n WHERE n.user_id = s.user_id AND n.lifted_at IS NULL)`, userIDs).
		Scan(&rows).Error
	if err != nil {
		return nil, wrap("bands", err)
	}
	for _, r := range rows {
		if b := bandOf(r.Band); b != BandNeutral {
			out[r.UserID] = b
		}
	}
	return out, nil
}

// StoredBand implements Store.
//
// cross-org: the subject's own score.
func (s gormStore) StoredBand(ctx context.Context, userID uuid.UUID) (Band, bool, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return 0, false, err
	}
	var bands []int16
	if err := c.Raw(`SELECT band FROM talent.scores WHERE user_id = ?`, userID).Scan(&bands).Error; err != nil {
		return 0, false, wrap("stored band", err)
	}
	if len(bands) == 0 {
		return BandNeutral, false, nil
	}
	return bandOf(bands[0]), true, nil
}

type scoreRow struct {
	UserID          uuid.UUID `gorm:"column:user_id"`
	RawCP           int       `gorm:"column:raw_cp"`
	PctBP           int       `gorm:"column:pct_bp"`
	Band            int16     `gorm:"column:band"`
	NSignals        int       `gorm:"column:n_signals"`
	CohortKey       string    `gorm:"column:cohort_key"`
	ConfigVersionID uuid.UUID `gorm:"column:config_version_id"`
	RunID           uuid.UUID `gorm:"column:run_id"`
	TopContributors string    `gorm:"column:top_contributors"`
	Neutralized     bool      `gorm:"column:neutralized"`
	CalculatedAt    time.Time `gorm:"column:calculated_at"`
}

const scoreCols = `user_id, raw_cp, round(pct_final * 10000)::int AS pct_bp, band, n_signals, cohort_key, config_version_id, run_id,
	top_contributors::text AS top_contributors, neutralized, calculated_at`

// score converts the row.
//
// cross-org: no statement.
func (r scoreRow) score() (Score, error) {
	sc := Score{UserID: r.UserID, RawCP: r.RawCP, PctBP: r.PctBP, Band: bandOf(r.Band), NSignals: r.NSignals, CohortKey: r.CohortKey,
		ConfigVersionID: r.ConfigVersionID, RunID: r.RunID, Neutralized: r.Neutralized, CalculatedAt: r.CalculatedAt}
	if err := json.Unmarshal([]byte(r.TopContributors), &sc.TopContributors); err != nil {
		return Score{}, fmt.Errorf("talent: top contributors of %s: %w", r.UserID, err)
	}
	return sc, nil
}

// Score implements Store.
//
// cross-org: the subject's own score.
func (s gormStore) Score(ctx context.Context, userID uuid.UUID) (Score, bool, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return Score{}, false, err
	}
	var rows []scoreRow
	if err := c.Raw(`SELECT `+scoreCols+` FROM talent.scores WHERE user_id = ?`, userID).Scan(&rows).Error; err != nil {
		return Score{}, false, wrap("score", err)
	}
	if len(rows) == 0 {
		return Score{}, false, nil
	}
	sc, err := rows[0].score()
	return sc, err == nil, err
}

// BandCounts implements Store.
//
// cross-org: aggregate counts of the auditor's distribution (no user row
// leaves the store).
func (s gormStore) BandCounts(ctx context.Context, cohortPrefix string) (map[int]int, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Band  int `gorm:"column:band"`
		Count int `gorm:"column:n"`
	}
	err = c.Raw(`SELECT s.band, count(*) AS n FROM talent.scores s
		WHERE NOT s.neutralized
		  AND NOT EXISTS (SELECT 1 FROM talent.neutralizations n WHERE n.user_id = s.user_id AND n.lifted_at IS NULL)
		  AND ( @p = '' OR s.cohort_key = @p OR starts_with(s.cohort_key, @p || ':'))
		GROUP BY s.band`, map[string]any{"p": cohortPrefix}).Scan(&rows).Error
	if err != nil {
		return nil, wrap("band counts", err)
	}
	out := map[int]int{}
	for _, r := range rows {
		out[r.Band] = r.Count
	}
	return out, nil
}

// ScoringUsers implements Store.
//
// cross-org: the nightly run's population (every talent subject).
func (s gormStore) ScoringUsers(ctx context.Context, since time.Time) ([]uuid.UUID, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	err = c.Raw(`SELECT user_id FROM talent.point_events WHERE occurred_at > ?
		UNION SELECT user_id FROM talent.scores
		UNION SELECT user_id FROM talent.cohorts
		ORDER BY user_id`, since).Scan(&ids).Error
	if err != nil {
		return nil, wrap("scoring users", err)
	}
	return ids, nil
}

// ActiveNeutralized implements Store.
//
// cross-org: the nightly run's batch, keyed by user IDs.
func (s gormStore) ActiveNeutralized(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(userIDs) == 0 {
		return out, nil
	}
	c, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	err = c.Raw(`SELECT DISTINCT user_id FROM talent.neutralizations WHERE user_id IN ? AND lifted_at IS NULL`, userIDs).
		Scan(&ids).Error
	if err != nil {
		return nil, wrap("active neutralized", err)
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// InsertRun implements Store. started_at is the run's clock (a business
// time of the run, like finished_at).
//
// cross-org: platform run log, no user data.
func (s gormStore) InsertRun(ctx context.Context, r ScoreRun) error {
	c, err := s.conn(ctx)
	if err != nil {
		return err
	}
	if err := c.Exec(`INSERT INTO talent.score_runs (id, config_version_id, started_at, status) VALUES ( ?, ?, ?, ?)`,
		r.ID, r.ConfigVersionID, r.StartedAt, RunRunning).Error; err != nil {
		return wrap("insert run", err)
	}
	return nil
}

// FinishRun implements Store.
//
// cross-org: platform run log, no user data.
func (s gormStore) FinishRun(ctx context.Context, r ScoreRun) error {
	c, err := s.conn(ctx)
	if err != nil {
		return err
	}
	res := c.Exec(`UPDATE talent.score_runs SET finished_at = ?, users_processed = ?, status = ?, error = ? WHERE id = ?`,
		r.FinishedAt, r.UsersProcessed, r.Status, nullArg(r.Error), r.ID)
	if res.Error != nil {
		return wrap("finish run", res.Error)
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("talent: finish run %s: no such run", r.ID)
	}
	return nil
}

// PutScores implements Store. calculated_at is the run's instant (a
// business time). The cohort row's updated_at is the touch trigger's.
//
// cross-org: the nightly run's batch, keyed by user IDs.
func (s gormStore) PutScores(ctx context.Context, scores []Score) error {
	c, err := s.conn(ctx)
	if err != nil {
		return err
	}
	for _, sc := range scores {
		top := sc.TopContributors
		if top == nil {
			top = []Contribution{}
		}
		b, err := json.Marshal(top)
		if err != nil {
			return fmt.Errorf("talent: top contributors: %w", err)
		}
		err = c.Exec(`INSERT INTO talent.scores (user_id, raw_cp, pct_final, band, n_signals, cohort_key, config_version_id, run_id,
			top_contributors, neutralized, calculated_at)
			VALUES ( ?, ?, CAST( ? AS numeric) / 10000, ?, ?, ?, ?, ?, CAST( ? AS jsonb), ?, ?)
			ON CONFLICT (user_id) DO UPDATE SET raw_cp = EXCLUDED.raw_cp, pct_final = EXCLUDED.pct_final, band = EXCLUDED.band,
			  n_signals = EXCLUDED.n_signals, cohort_key = EXCLUDED.cohort_key, config_version_id = EXCLUDED.config_version_id,
			  run_id = EXCLUDED.run_id, top_contributors = EXCLUDED.top_contributors, neutralized = EXCLUDED.neutralized,
			  calculated_at = EXCLUDED.calculated_at`,
			sc.UserID, sc.RawCP, sc.PctBP, int16(sc.Band), sc.NSignals, sc.CohortKey, sc.ConfigVersionID, sc.RunID, string(b),
			sc.Neutralized, sc.CalculatedAt).Error
		if err != nil {
			return wrap("put score", err)
		}
		if err := c.Exec(`INSERT INTO talent.cohorts (user_id, cohort_key) VALUES ( ?, ?)
			ON CONFLICT (user_id) DO UPDATE SET cohort_key = EXCLUDED.cohort_key`, sc.UserID, sc.CohortKey).Error; err != nil {
			return wrap("put cohort", err)
		}
	}
	return nil
}

// DeleteScores implements Store.
//
// cross-org: the nightly run's batch, keyed by user IDs.
func (s gormStore) DeleteScores(ctx context.Context, userIDs []uuid.UUID) (int, int, error) {
	if len(userIDs) == 0 {
		return 0, 0, nil
	}
	c, err := s.conn(ctx)
	if err != nil {
		return 0, 0, err
	}
	var n [2]int
	for i, table := range []string{"scores", "cohorts"} {
		res := c.Exec(`DELETE FROM talent.`+table+` WHERE user_id IN ?`, userIDs)
		if res.Error != nil {
			return 0, 0, wrap("delete "+table, res.Error)
		}
		n[i] = int(res.RowsAffected)
	}
	return n[0], n[1], nil
}

// PurgeScores implements Store: one statement picks the users and deletes
// both tables' rows of them (data-modifying CTEs see the same snapshot).
//
// cross-org: collection is off for every user (T-324).
func (s gormStore) PurgeScores(ctx context.Context, limit int) (int, int, error) {
	if limit < 1 || limit > 1000 {
		return 0, 0, fmt.Errorf("talent: purge scores: batch size %d outside 1..1000", limit)
	}
	c, err := s.conn(ctx)
	if err != nil {
		return 0, 0, err
	}
	var row struct {
		Scores  int `gorm:"column:scores"`
		Cohorts int `gorm:"column:cohorts"`
	}
	err = c.Raw(`WITH u AS (
			SELECT user_id FROM talent.scores UNION SELECT user_id FROM talent.cohorts ORDER BY user_id LIMIT ?
		), s AS (
			DELETE FROM talent.scores WHERE user_id IN (SELECT user_id FROM u) RETURNING 1
		), c AS (
			DELETE FROM talent.cohorts WHERE user_id IN (SELECT user_id FROM u) RETURNING 1
		)
		SELECT (SELECT count(*) FROM s) AS scores, (SELECT count(*) FROM c) AS cohorts`, limit).Scan(&row).Error
	if err != nil {
		return 0, 0, wrap("purge scores", err)
	}
	return row.Scores, row.Cohorts, nil
}

// HasData implements Store.
//
// cross-org: the subject's own rows.
func (s gormStore) HasData(ctx context.Context, userID uuid.UUID) (bool, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return false, err
	}
	var has bool
	err = c.Raw(`SELECT EXISTS (SELECT 1 FROM talent.point_events WHERE user_id = @u)
		OR EXISTS (SELECT 1 FROM talent.daily_counters WHERE user_id = @u)
		OR EXISTS (SELECT 1 FROM talent.neutralizations WHERE user_id = @u)
		OR EXISTS (SELECT 1 FROM talent.cohorts WHERE user_id = @u)
		OR EXISTS (SELECT 1 FROM talent.scores WHERE user_id = @u)`, map[string]any{"u": userID}).Scan(&has).Error
	if err != nil {
		return false, wrap("has data", err)
	}
	return has, nil
}

// EraseUser implements Store. The ledger function is called only when the
// user has ledger rows, so an account without any never depends on the
// erasure request's state.
//
// cross-org: the subject's own rows.
func (s gormStore) EraseUser(ctx context.Context, userID uuid.UUID) (int, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, table := range []string{"daily_counters", "cohorts", "scores", "neutralizations"} {
		res := c.Exec(`DELETE FROM talent.`+table+` WHERE user_id = ?`, userID)
		if res.Error != nil {
			return 0, wrap("erase "+table, res.Error)
		}
		total += int(res.RowsAffected)
	}
	var ledger bool
	if err := c.Raw(`SELECT EXISTS (SELECT 1 FROM talent.point_events WHERE user_id = ?)`, userID).Scan(&ledger).Error; err != nil {
		return 0, wrap("erase ledger check", err)
	}
	if ledger {
		var n int64
		if err := c.Raw(`SELECT talent.point_events_erase_user( ? )`, userID).Scan(&n).Error; err != nil {
			return 0, wrap("erase ledger", err)
		}
		total += int(n)
	}
	return total, nil
}

// ---------------------------------------------------------------- retention

// purgeFunc calls a SECURITY DEFINER purge function of 000121
// (talent.point_events_purge, talent.config_versions_purge): app_talent
// holds EXECUTE on them, no role holds DELETE on the append-only tables.
//
// cross-org: retention spans every user.
func (s gormStore) purgeFunc(ctx context.Context, fn string, cutoff time.Time, limit int, dry bool) (int, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := c.Raw(`SELECT talent.`+fn+`( ?, ?, ?)`, cutoff, limit, dry).Scan(&n).Error; err != nil {
		return 0, wrap(fn, err)
	}
	return int(n), nil
}

// purgeRows counts (dry) or deletes one batch of the rows of table that
// match where, oldest first by order; key lists the key columns.
//
// cross-org: retention spans every user.
func (s gormStore) purgeRows(ctx context.Context, op, table, key, where, order string, limit int, dry bool, args ...any) (int, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return 0, err
	}
	if dry {
		var n int64
		if err := c.Raw(`SELECT count(*) FROM talent.`+table+` WHERE `+where, args...).Scan(&n).Error; err != nil {
			return 0, wrap(op, err)
		}
		return int(n), nil
	}
	res := c.Exec(`DELETE FROM talent.`+table+` WHERE (`+key+`) IN (SELECT `+key+` FROM talent.`+table+` WHERE `+where+
		` ORDER BY `+order+` LIMIT ?)`, append(args, limit)...)
	if res.Error != nil {
		return 0, wrap(op, res.Error)
	}
	return int(res.RowsAffected), nil
}

// PurgeLedger implements Store.
//
// cross-org: retention spans every user.
func (s gormStore) PurgeLedger(ctx context.Context, cutoff time.Time, limit int, dry bool) (int, error) {
	return s.purgeFunc(ctx, "point_events_purge", cutoff, limit, dry)
}

// PurgeConfigVersions implements Store.
//
// cross-org: platform configuration, no org data.
func (s gormStore) PurgeConfigVersions(ctx context.Context, cutoff time.Time, limit int, dry bool) (int, error) {
	return s.purgeFunc(ctx, "config_versions_purge", cutoff, limit, dry)
}

// PurgeCounters implements Store.
//
// cross-org: retention spans every user.
func (s gormStore) PurgeCounters(ctx context.Context, before civil.Date, limit int, dry bool) (int, error) {
	return s.purgeRows(ctx, "purge counters", "daily_counters", "user_id, day, signal_code", "day < ?",
		"day, user_id, signal_code", limit, dry, before)
}

// PurgeRuns implements Store.
//
// cross-org: platform run log, no user data.
func (s gormStore) PurgeRuns(ctx context.Context, cutoff time.Time, limit int, dry bool) (int, error) {
	return s.purgeRows(ctx, "purge runs", "score_runs", "id", "started_at < ?", "started_at, id", limit, dry, cutoff)
}

// PurgeNeutralizations implements Store.
//
// cross-org: retention spans every user.
func (s gormStore) PurgeNeutralizations(ctx context.Context, cutoff time.Time, limit int, dry bool) (int, error) {
	return s.purgeRows(ctx, "purge neutralizations", "neutralizations", "id", "lifted_at IS NOT NULL AND lifted_at < ?",
		"lifted_at, id", limit, dry, cutoff)
}

// nullArg maps "" to a NULL raw SQL argument (untyped nil; db.Nullable
// gives a *string column value).
func nullArg(s string) any {
	if s == "" {
		return nil
	}
	return s
}
