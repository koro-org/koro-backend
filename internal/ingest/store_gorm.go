package ingest

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// NewGormStore returns the PostgreSQL store over the ingest tables (000187).
// None of them is an org's tenant data: the sources, runs and imported rows
// are the platform's (admin surface, worker) or public; every method says
// so with a "cross-org:" note (05 §5.5).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// Column lists.
const (
	sourceCols = `id, kind, adapter, url, host, label, university_id, club_org_id, parent_id, interval_seconds, enabled,
		paused_reason, next_run_at, etag, last_modified, content_hash, fail_count, zero_yield_runs, last_error,
		last_status, last_yield, last_fetched_at, last_ok_at, created_at, updated_at`
	runCols = `id, source_id, started_at, finished_at, outcome, http_status, bytes, found, created, updated, rejected,
		input_tokens, output_tokens, error_code`
	clubCols = `club_org_id, university_id, source_id, name_key, source_category, needs_review, contact_email,
		first_seen_at, last_seen_at, missing_since, created_at, updated_at`
	eventCols = `id, source_id, university_id, club_org_id, status, hidden_reason, title, starts_at, ends_at, all_day,
		location_text, category, source_url, evidence, confidence, fingerprint, promoted_event_id, expires_at,
		last_seen_at, created_at, updated_at`
)

// constraints maps the named constraints of 000187 onto module errors.
var constraints = db.Constraints{
	"ingest_sources_url_uq":                  ErrSourceExists,
	"ingest_sources_university_id_fkey":      problem.ErrNotFound,
	"ingest_sources_club_org_id_fkey":        problem.ErrNotFound,
	"ingest_sources_parent_id_fkey":          problem.ErrNotFound,
	"imported_events_fingerprint_uq":         problem.ErrConflict,
	"imported_clubs_name_uq":                 problem.ErrConflict,
	"imported_events_promoted_event_id_fkey": problem.ErrNotFound,
}

func wrap(op string, err error) error {
	return fmt.Errorf("ingest: %s: %w", op, constraints.Translate(err))
}

// take reads one row of q into T or errNotFound.
func take[T any](op, cols string, q *gorm.DB) (T, error) {
	var v T
	err := q.Select(cols).Take(&v).Error
	switch {
	case db.IsNotFound(err):
		return v, errNotFound
	case err != nil:
		return v, wrap(op, err)
	}
	return v, nil
}

func locked(q *gorm.DB) *gorm.DB {
	return q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
}

// ---- sources

// InsertSource implements Store.
//
// cross-org: platform sources (admin surface, worker).
func (s gormStore) InsertSource(ctx context.Context, r Source) (Source, bool, error) {
	res := db.Conn(ctx, s.orm).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "url"}}, DoNothing: true}).
		Omit("created_at", "updated_at").Create(&r)
	if res.Error != nil {
		return Source{}, false, wrap("insert source", res.Error)
	}
	row, err := take[Source]("get source", sourceCols, db.Conn(ctx, s.orm).Where("url = ?", r.URL))
	return row, res.RowsAffected == 1, err
}

// GetSource implements Store.
//
// cross-org: platform sources.
func (s gormStore) GetSource(ctx context.Context, id uuid.UUID) (Source, error) {
	return take[Source]("get source", sourceCols, db.Conn(ctx, s.orm).Where("id = ?", id))
}

// LockSource implements Store.
//
// cross-org: platform sources.
func (s gormStore) LockSource(ctx context.Context, id uuid.UUID) (Source, error) {
	return take[Source]("lock source", sourceCols, locked(db.Conn(ctx, s.orm)).Where("id = ?", id))
}

// ListSources implements Store.
//
// cross-org: the admin list of every source.
func (s gormStore) ListSources(ctx context.Context, f SourceFilter, after *uuid.UUID, limit int) ([]Source, error) {
	q := db.Conn(ctx, s.orm).Model(&Source{}).Select(sourceCols)
	if f.UniversityID != nil {
		q = q.Where("university_id = ?", *f.UniversityID)
	}
	if f.ClubOrgID != nil {
		q = q.Where("club_org_id = ?", *f.ClubOrgID)
	}
	if f.Kind != "" {
		q = q.Where("kind = ?", string(f.Kind))
	}
	if f.Enabled != nil {
		q = q.Where("enabled = ?", *f.Enabled)
	}
	if after != nil {
		q = q.Where("id > ?", *after)
	}
	var out []Source
	if err := q.Order("id").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list sources", err)
	}
	return out, nil
}

// DueSources implements Store.
//
// cross-org: the worker's due sources.
func (s gormStore) DueSources(ctx context.Context, now time.Time, limit int) ([]Source, error) {
	var out []Source
	err := db.Conn(ctx, s.orm).Model(&Source{}).Select(sourceCols).
		Where("enabled AND next_run_at <= ?", now).Order("next_run_at, id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, wrap("due sources", err)
	}
	return out, nil
}

// HostLastFetched implements Store.
//
// cross-org: the per-host politeness gap.
func (s gormStore) HostLastFetched(ctx context.Context, hosts []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if len(hosts) == 0 {
		return out, nil
	}
	var rows []struct {
		Host string    `gorm:"column:host"`
		Last time.Time `gorm:"column:last"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT host, max(last_fetched_at) AS last FROM ingest_sources
		WHERE host IN ? AND last_fetched_at IS NOT NULL GROUP BY host`, hosts).Scan(&rows).Error
	if err != nil {
		return nil, wrap("host last fetched", err)
	}
	for _, r := range rows {
		out[r.Host] = r.Last
	}
	return out, nil
}

// UpdateSource implements Store.
//
// cross-org: platform sources.
func (s gormStore) UpdateSource(ctx context.Context, r Source) error {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE ingest_sources SET interval_seconds = @interval, enabled = @enabled,
		paused_reason = @paused, next_run_at = @next, etag = @etag, last_modified = @last_modified,
		content_hash = @hash, fail_count = @fails, zero_yield_runs = @zero, last_error = @last_error,
		last_status = @last_status, last_yield = @last_yield, last_fetched_at = @fetched, last_ok_at = @ok
		WHERE id = @id`, map[string]any{
		"id": r.ID, "interval": r.IntervalSeconds, "enabled": r.Enabled, "paused": pausedArg(r.PausedReason),
		"next": r.NextRunAt, "etag": r.ETag, "last_modified": r.LastModified, "hash": r.ContentHash,
		"fails": r.FailCount, "zero": r.ZeroYieldRuns, "last_error": r.LastError, "last_status": r.LastStatus,
		"last_yield": r.LastYield, "fetched": r.LastFetchedAt, "ok": r.LastOKAt,
	})
	if res.Error != nil {
		return wrap("update source", res.Error)
	}
	if res.RowsAffected == 0 {
		return errNotFound
	}
	return nil
}

func pausedArg(p *PausedReason) *string {
	if p == nil {
		return nil
	}
	v := string(*p)
	return &v
}

// PauseClubSources implements Store.
//
// cross-org: the sources of one club (club_org_id is not a tenant scope here).
func (s gormStore) PauseClubSources(ctx context.Context, clubOrgID uuid.UUID, reason PausedReason) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE ingest_sources SET enabled = false, paused_reason = ?
		WHERE club_org_id = ? AND enabled`, string(reason), clubOrgID)
	if res.Error != nil {
		return 0, wrap("pause club sources", res.Error)
	}
	return int(res.RowsAffected), nil
}

// DeleteFetchedChildren implements Store.
//
// cross-org: the worker's purge.
func (s gormStore) DeleteFetchedChildren(ctx context.Context, before time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM ingest_sources WHERE id IN (
		SELECT id FROM ingest_sources WHERE parent_id IS NOT NULL AND interval_seconds = 0
		AND last_fetched_at < ? ORDER BY last_fetched_at LIMIT ? FOR UPDATE SKIP LOCKED)`, before, limit)
	if res.Error != nil {
		return 0, wrap("delete fetched children", res.Error)
	}
	return int(res.RowsAffected), nil
}

// ---- runs

// InsertRun implements Store.
//
// cross-org: the worker's run log.
func (s gormStore) InsertRun(ctx context.Context, r Run) error {
	if err := db.Conn(ctx, s.orm).Create(&r).Error; err != nil {
		return wrap("insert run", err)
	}
	return nil
}

// ListRuns implements Store.
//
// cross-org: the admin's view of one source.
func (s gormStore) ListRuns(ctx context.Context, sourceID uuid.UUID, limit int) ([]Run, error) {
	var out []Run
	err := db.Conn(ctx, s.orm).Model(&Run{}).Select(runCols).Where("source_id = ?", sourceID).
		Order("started_at DESC, id DESC").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, wrap("list runs", err)
	}
	return out, nil
}

// DeleteRunsBefore implements Store. ingest_runs is append-only (app_rw has
// no UPDATE on it), so the batch takes no row lock: SELECT … FOR UPDATE needs
// the UPDATE privilege (the unsubscribe_links / iys_netgsm_events purges do
// the same). Nothing else writes an old run, and purge_expired is a unique job.
//
// cross-org: the worker's purge.
func (s gormStore) DeleteRunsBefore(ctx context.Context, before time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM ingest_runs WHERE id IN (
		SELECT id FROM ingest_runs WHERE started_at < ? ORDER BY started_at, id LIMIT ?)`, before, limit)
	if res.Error != nil {
		return 0, wrap("delete runs", res.Error)
	}
	return int(res.RowsAffected), nil
}

// ---- imported clubs

// GetImportedClub implements Store.
//
// cross-org: the platform's import record of a club.
func (s gormStore) GetImportedClub(ctx context.Context, clubOrgID uuid.UUID) (ImportedClub, error) {
	return take[ImportedClub]("get imported club", clubCols, db.Conn(ctx, s.orm).Where("club_org_id = ?", clubOrgID))
}

// FindImportedClub implements Store.
//
// cross-org: the platform's import record of a club, by its key.
func (s gormStore) FindImportedClub(ctx context.Context, universityID uuid.UUID, nameKey string) (ImportedClub, error) {
	return take[ImportedClub]("find imported club", clubCols,
		db.Conn(ctx, s.orm).Where("university_id = ? AND name_key = ?", universityID, nameKey))
}

// UpsertImportedClub implements Store.
//
// cross-org: the platform's import record of a club.
func (s gormStore) UpsertImportedClub(ctx context.Context, c ImportedClub) error {
	err := db.Conn(ctx, s.orm).Exec(`INSERT INTO imported_clubs (club_org_id, university_id, source_id, name_key,
		source_category, needs_review, contact_email, first_seen_at, last_seen_at)
		VALUES ( @club, @uni, @source, @key, @category, @review, @email, @seen, @seen)
		ON CONFLICT (club_org_id) DO UPDATE SET source_id = EXCLUDED.source_id, name_key = EXCLUDED.name_key,
		source_category = EXCLUDED.source_category, last_seen_at = EXCLUDED.last_seen_at, missing_since = NULL,
		contact_email = COALESCE(EXCLUDED.contact_email, imported_clubs.contact_email)`, map[string]any{
		"club": c.ClubOrgID, "uni": c.UniversityID, "source": c.SourceID, "key": c.NameKey,
		"category": c.SourceCategory, "review": c.NeedsReview, "email": c.ContactEmail, "seen": c.LastSeenAt,
	}).Error
	if err != nil {
		return wrap("upsert imported club", err)
	}
	return nil
}

// MarkMissingClubs implements Store.
//
// cross-org: the clubs of one official list.
func (s gormStore) MarkMissingClubs(ctx context.Context, sourceID uuid.UUID, seenBefore, now time.Time) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE imported_clubs SET missing_since = ?
		WHERE source_id = ? AND last_seen_at < ? AND missing_since IS NULL`, now, sourceID, seenBefore)
	if res.Error != nil {
		return 0, wrap("mark missing clubs", res.Error)
	}
	return int(res.RowsAffected), nil
}

// ListImportedClubs implements Store.
//
// cross-org: the admin's review list.
func (s gormStore) ListImportedClubs(ctx context.Context, f ImportedClubFilter, after *uuid.UUID, limit int) ([]ImportedClub, error) {
	q := db.Conn(ctx, s.orm).Model(&ImportedClub{}).Select(clubCols)
	if f.UniversityID != nil {
		q = q.Where("university_id = ?", *f.UniversityID)
	}
	if f.NeedsReview != nil {
		q = q.Where("needs_review = ?", *f.NeedsReview)
	}
	if after != nil {
		q = q.Where("club_org_id > ?", *after)
	}
	var out []ImportedClub
	if err := q.Order("club_org_id").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list imported clubs", err)
	}
	return out, nil
}

// MarkClubReviewed implements Store.
//
// cross-org: the admin's review of a club's category.
func (s gormStore) MarkClubReviewed(ctx context.Context, clubOrgID uuid.UUID) error {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE imported_clubs SET needs_review = false WHERE club_org_id = ?`, clubOrgID)
	if res.Error != nil {
		return wrap("mark club reviewed", res.Error)
	}
	if res.RowsAffected == 0 {
		return errNotFound
	}
	return nil
}

// ---- imported events

// EventByFingerprint implements Store.
//
// cross-org: public imported events.
func (s gormStore) EventByFingerprint(ctx context.Context, fp []byte) (ImportedEvent, error) {
	return take[ImportedEvent]("event by fingerprint", eventCols, db.Conn(ctx, s.orm).Where("fingerprint = ?", fp))
}

// InsertEvent implements Store.
//
// cross-org: public imported events.
func (s gormStore) InsertEvent(ctx context.Context, e ImportedEvent) error {
	if err := db.Conn(ctx, s.orm).Omit("created_at", "updated_at").Create(&e).Error; err != nil {
		return wrap("insert imported event", err)
	}
	return nil
}

// RefreshEvent implements Store.
//
// cross-org: public imported events.
func (s gormStore) RefreshEvent(ctx context.Context, e ImportedEvent) error {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE imported_events SET ends_at = @ends, location_text = @loc,
		category = @cat, expires_at = @expires, last_seen_at = @seen WHERE id = @id`, map[string]any{
		"id": e.ID, "ends": e.EndsAt, "loc": e.LocationText, "cat": string(e.Category), "expires": e.ExpiresAt,
		"seen": e.LastSeenAt,
	})
	if res.Error != nil {
		return wrap("refresh imported event", res.Error)
	}
	if res.RowsAffected == 0 {
		return errNotFound
	}
	return nil
}

// GetEvent implements Store.
//
// cross-org: public imported events.
func (s gormStore) GetEvent(ctx context.Context, id uuid.UUID) (ImportedEvent, error) {
	return take[ImportedEvent]("get imported event", eventCols, db.Conn(ctx, s.orm).Where("id = ?", id))
}

// LockEvent implements Store.
//
// cross-org: public imported events.
func (s gormStore) LockEvent(ctx context.Context, id uuid.UUID) (ImportedEvent, error) {
	return take[ImportedEvent]("lock imported event", eventCols, locked(db.Conn(ctx, s.orm)).Where("id = ?", id))
}

// SetEventStatus implements Store.
//
// cross-org: public imported events.
func (s gormStore) SetEventStatus(ctx context.Context, id uuid.UUID, st Status, reason *HiddenReason, promoted *uuid.UUID) error {
	var r *string
	if reason != nil {
		v := string(*reason)
		r = &v
	}
	res := db.Conn(ctx, s.orm).Exec(`UPDATE imported_events SET status = ?, hidden_reason = ?, promoted_event_id = ?
		WHERE id = ?`, string(st), r, promoted, id)
	if res.Error != nil {
		return wrap("set imported event status", res.Error)
	}
	if res.RowsAffected == 0 {
		return errNotFound
	}
	return nil
}

// DeleteEvent implements Store.
//
// cross-org: public imported events.
func (s gormStore) DeleteEvent(ctx context.Context, id uuid.UUID) error {
	if err := db.Conn(ctx, s.orm).Exec(`DELETE FROM imported_events WHERE id = ?`, id).Error; err != nil {
		return wrap("delete imported event", err)
	}
	return nil
}

// ListEvents implements Store.
//
// cross-org: public imported events (and the admin's list).
func (s gormStore) ListEvents(ctx context.Context, f EventFilter, after *EventAfter, limit int) ([]ImportedEvent, error) {
	q := db.Conn(ctx, s.orm).Model(&ImportedEvent{}).Select(eventCols)
	if len(f.Statuses) > 0 {
		sts := make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			sts[i] = string(st)
		}
		q = q.Where("status IN ?", sts)
	}
	if f.UniversityID != nil {
		q = q.Where("university_id = ?", *f.UniversityID)
	}
	if f.ClubOrgID != nil {
		q = q.Where("club_org_id = ?", *f.ClubOrgID)
	}
	if f.SourceID != nil {
		q = q.Where("source_id = ?", *f.SourceID)
	}
	if f.From != nil {
		q = q.Where("starts_at >= ?", *f.From)
	}
	if f.To != nil {
		q = q.Where("starts_at < ?", *f.To)
	}
	if f.EndsAfter != nil {
		q = q.Where("COALESCE(ends_at, starts_at) > ?", *f.EndsAfter)
	}
	if after != nil {
		q = q.Where("( starts_at, id) > ( ?, ?)", after.StartsAt, after.ID)
	}
	var out []ImportedEvent
	if err := q.Order("starts_at, id").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list imported events", err)
	}
	return out, nil
}

// CountClubEvents implements Store.
//
// cross-org: the imported events of one club (club_org_id is not a tenant scope here).
func (s gormStore) CountClubEvents(ctx context.Context, clubOrgID uuid.UUID, st Status, now time.Time) (int, error) {
	var n int64
	err := db.Conn(ctx, s.orm).Model(&ImportedEvent{}).
		Where("club_org_id = ? AND status = ? AND COALESCE(ends_at, starts_at) > ?", clubOrgID, string(st), now).
		Count(&n).Error
	if err != nil {
		return 0, wrap("count club events", err)
	}
	return int(n), nil
}

// DeleteExpiredEvents implements Store.
//
// cross-org: the worker's purge.
func (s gormStore) DeleteExpiredEvents(ctx context.Context, now time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM imported_events WHERE id IN (
		SELECT id FROM imported_events WHERE expires_at <= ? ORDER BY expires_at LIMIT ? FOR UPDATE SKIP LOCKED)`, now, limit)
	if res.Error != nil {
		return 0, wrap("delete expired imported events", res.Error)
	}
	return int(res.RowsAffected), nil
}
