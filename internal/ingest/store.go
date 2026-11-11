package ingest

import (
	"context"
	"errors"
	"time"
	"uuid"
)

// ErrNotFound is returned by the stores for a missing row; the service maps
// it to the 404 of the operation.
var errNotFound = errors.New("ingest: not found")

// Store is the persistence of the ingest module, split by aggregate (05 §2.2:
// the sources and their runs, the imported clubs, the imported events). The
// GORM store (store_gorm.go) and the in-memory store (store_mem.go) pass the
// same contract suite (store_contract_test.go). Every store method joins the
// transaction carried by ctx (db.Conn).
type Store interface {
	SourceStore
	RunStore
	ClubStore
	EventStore
}

// SourceFilter narrows ListSources (adminListIngestSources).
type SourceFilter struct {
	UniversityID *uuid.UUID
	ClubOrgID    *uuid.UUID
	Kind         Kind
	Enabled      *bool
}

// SourceStore persists ingest_sources.
type SourceStore interface {
	// InsertSource writes s unless a source with its URL exists; created
	// reports which, and the stored row is returned either way.
	InsertSource(ctx context.Context, s Source) (row Source, created bool, err error)
	// GetSource returns a source or errNotFound.
	GetSource(ctx context.Context, id uuid.UUID) (Source, error)
	// LockSource returns a source FOR UPDATE (in a transaction) or errNotFound.
	LockSource(ctx context.Context, id uuid.UUID) (Source, error)
	// ListSources returns the sources after the ID after (creation order).
	ListSources(ctx context.Context, f SourceFilter, after *uuid.UUID, limit int) ([]Source, error)
	// DueSources returns enabled sources whose next_run_at is at or before
	// now, earliest first.
	DueSources(ctx context.Context, now time.Time, limit int) ([]Source, error)
	// HostLastFetched returns the latest last_fetched_at of the sources of
	// each host (a host never fetched has no key): the per-host gap.
	HostLastFetched(ctx context.Context, hosts []string) (map[string]time.Time, error)
	// UpdateSource writes the mutable columns of s (schedule, state,
	// counters, enabled/paused_reason).
	UpdateSource(ctx context.Context, s Source) error
	// PauseClubSources disables the enabled sources of a club with reason
	// and returns how many it changed.
	PauseClubSources(ctx context.Context, clubOrgID uuid.UUID, reason PausedReason) (int, error)
	// DeleteFetchedChildren deletes up to limit one-off sources a sitemap
	// named (parent_id set, interval 0) last fetched before before.
	DeleteFetchedChildren(ctx context.Context, before time.Time, limit int) (int, error)
}

// RunStore persists ingest_runs.
type RunStore interface {
	// InsertRun writes one run.
	InsertRun(ctx context.Context, r Run) error
	// ListRuns returns the latest runs of a source, newest first.
	ListRuns(ctx context.Context, sourceID uuid.UUID, limit int) ([]Run, error)
	// DeleteRunsBefore deletes up to limit runs started before before.
	DeleteRunsBefore(ctx context.Context, before time.Time, limit int) (int, error)
}

// ImportedClubFilter narrows ListImportedClubs.
type ImportedClubFilter struct {
	UniversityID *uuid.UUID
	NeedsReview  *bool
}

// ClubStore persists imported_clubs.
type ClubStore interface {
	// GetImportedClub returns the row of a club or errNotFound.
	GetImportedClub(ctx context.Context, clubOrgID uuid.UUID) (ImportedClub, error)
	// FindImportedClub returns the row of a university's club by its name
	// key (directory.ClubNameKey) or errNotFound.
	FindImportedClub(ctx context.Context, universityID uuid.UUID, nameKey string) (ImportedClub, error)
	// ListImportedClubs returns the rows matching f after the club after
	// (by club_org_id).
	ListImportedClubs(ctx context.Context, f ImportedClubFilter, after *uuid.UUID, limit int) ([]ImportedClub, error)
	// MarkClubReviewed clears needs_review of a club (errNotFound when the
	// club has no row).
	MarkClubReviewed(ctx context.Context, clubOrgID uuid.UUID) error
	// UpsertImportedClub inserts c or updates the row of its club (source,
	// key, labels, e-mail, last_seen_at; missing_since cleared; first_seen_at
	// and needs_review kept once set unless c clears review).
	UpsertImportedClub(ctx context.Context, c ImportedClub) error
	// MarkMissingClubs sets missing_since = now on the clubs of a source not
	// seen since seenBefore (the list dropped them) and returns how many.
	MarkMissingClubs(ctx context.Context, sourceID uuid.UUID, seenBefore, now time.Time) (int, error)
}

// EventAfter is the keyset position of the event lists (starts_at, id).
type EventAfter struct {
	StartsAt time.Time
	ID       uuid.UUID
}

// EventFilter narrows the imported event lists. Statuses empty: any.
type EventFilter struct {
	Statuses     []Status
	UniversityID *uuid.UUID
	ClubOrgID    *uuid.UUID
	SourceID     *uuid.UUID
	From         *time.Time // starts_at ≥ From
	To           *time.Time // starts_at < To
	// EndsAfter keeps events whose end (ends_at, else starts_at) is after it.
	EndsAfter *time.Time
}

// EventStore persists imported_events.
type EventStore interface {
	// EventByFingerprint returns the event of a fingerprint or errNotFound.
	EventByFingerprint(ctx context.Context, fp []byte) (ImportedEvent, error)
	// InsertEvent writes a new event.
	InsertEvent(ctx context.Context, e ImportedEvent) error
	// RefreshEvent updates the facts a later sighting may complete (ends_at,
	// location_text, category, expires_at) and last_seen_at.
	RefreshEvent(ctx context.Context, e ImportedEvent) error
	// GetEvent returns an event or errNotFound.
	GetEvent(ctx context.Context, id uuid.UUID) (ImportedEvent, error)
	// LockEvent returns an event FOR UPDATE (in a transaction) or errNotFound.
	LockEvent(ctx context.Context, id uuid.UUID) (ImportedEvent, error)
	// SetEventStatus writes status, hidden_reason and promoted_event_id.
	SetEventStatus(ctx context.Context, id uuid.UUID, st Status, reason *HiddenReason, promoted *uuid.UUID) error
	// DeleteEvent deletes an event (no error when it is gone).
	DeleteEvent(ctx context.Context, id uuid.UUID) error
	// ListEvents returns the events matching f after after, by (starts_at, id).
	ListEvents(ctx context.Context, f EventFilter, after *EventAfter, limit int) ([]ImportedEvent, error)
	// CountClubEvents counts a club's events in status st ending after now.
	CountClubEvents(ctx context.Context, clubOrgID uuid.UUID, st Status, now time.Time) (int, error)
	// DeleteExpiredEvents deletes up to limit events whose expires_at is at
	// or before now.
	DeleteExpiredEvents(ctx context.Context, now time.Time, limit int) (int, error)
}
