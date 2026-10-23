package events

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of events (05 §2.2) over events,
// event_forms, event_form_versions, form_lint_rules and event_staff (02
// §11.1–11.5). The GORM store (store_gorm.go) and the in-memory store
// (store_mem.go) pass the same contract test (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any; the service
// draws the boundaries.
//
// Org scope: an event is addressed by its id (GET /events/{event_id}); the
// service reads it with GetEvent/LockEvent, takes org_id from the row and
// passes that org to every later call (06 H-38). event_forms and
// event_staff copy org_id from their events row, never from the caller:
// InsertForm and InsertStaff fail with ErrNotFound when the event is not
// the given org's. Registrations and tickets (BE-028) are RegistrationStore,
// check-ins and attendance (BE-031) CheckinStore, exports (BE-050)
// ExportStore.
type Store interface {
	RegistrationStore
	CheckinStore
	RetentionStore
	ExportStore
	ParticipationStore

	// InsertEvent writes a new event and returns it as stored (created_at
	// and updated_at from the database). It reports false and writes
	// nothing when the slug is taken in the org (ON CONFLICT DO NOTHING:
	// the transaction stays usable, so the caller can try another slug).
	InsertEvent(ctx context.Context, e Event) (Event, bool, error)
	// GetEvent returns the event or ErrNotFound.
	GetEvent(ctx context.Context, id uuid.UUID) (Event, error)
	// LockEvent is GetEvent with the row locked for the transaction.
	LockEvent(ctx context.Context, id uuid.UUID) (Event, error)
	// UpdateEvent writes the mutable columns of e (scoped to e.OrgID) and
	// returns the stored row: updated_at changes on every call, so the ETag
	// does too (04 §1.13). ErrNotFound when the event is gone; ErrSlugTaken
	// when the new slug is taken in the org.
	UpdateEvent(ctx context.Context, e Event) (Event, error)
	// DeleteDraft deletes a draft event of orgID with its form draft and
	// staff rows. ErrNotFound when no draft event id belongs to orgID; a
	// form with a published version cannot go (ErrInvalidState).
	DeleteDraft(ctx context.Context, orgID, id uuid.UUID) error
	// ListEvents returns up to limit published events matching f after the
	// position after (nil: the first page), in f.Sort order. Only events the
	// viewer f.Viewer may see are returned (03 §3.6).
	ListEvents(ctx context.Context, f ListFilter, after *After, limit int) ([]Event, error)
	// ListOrgEvents returns up to limit events of orgID (drafts included)
	// matching f after the position after, by starts_at then id.
	ListOrgEvents(ctx context.Context, orgID uuid.UUID, f OrgListFilter, after *After, limit int) ([]Event, error)

	// InsertForm writes the form row of an event of orgID (org_id from the
	// events row). ErrNotFound when the event is not orgID's.
	InsertForm(ctx context.Context, orgID uuid.UUID, f Form) error
	// HasPublishedForm reports whether the form of event id (of orgID) has
	// a current published version.
	HasPublishedForm(ctx context.Context, orgID, id uuid.UUID) (bool, error)

	// GetForm returns the form row of event eventID of orgID or ErrNotFound
	// (BE-026).
	GetForm(ctx context.Context, orgID, eventID uuid.UUID) (Form, error)
	// LockForm is GetForm with the row locked for the transaction: the
	// draft and the version number change under it.
	LockForm(ctx context.Context, orgID, eventID uuid.UUID) (Form, error)
	// UpdateFormDraft replaces the draft schema of form formID of orgID and
	// returns the row (updated_at moves, so the ETag does). ErrNotFound
	// when the form is not orgID's.
	UpdateFormDraft(ctx context.Context, orgID, formID uuid.UUID, schema JSON, by uuid.UUID) (Form, error)
	// InsertFormVersion writes the next version of form v.FormID of orgID:
	// version_no is one more than the highest, event_id comes from the
	// event_forms row (06 H-38; v.EventID and v.VersionNo are ignored). It
	// returns the stored row; ErrNotFound when the form is not orgID's.
	InsertFormVersion(ctx context.Context, orgID uuid.UUID, v FormVersion) (FormVersion, error)
	// SetCurrentVersion points form formID of orgID at one of its own
	// versions and returns the row; ErrNotFound otherwise.
	SetCurrentVersion(ctx context.Context, orgID, formID, versionID uuid.UUID) (Form, error)
	// GetFormVersion returns version id of a form of orgID or ErrNotFound.
	GetFormVersion(ctx context.Context, orgID, id uuid.UUID) (FormVersion, error)
	// ListFormVersions returns the versions of form formID of orgID by
	// version_no.
	ListFormVersions(ctx context.Context, orgID, formID uuid.UUID) ([]FormVersion, error)
	// HasRegistrations reports whether event eventID of orgID has any
	// registration (02 §11.6, any status): R-003 §2.5.2 forbids a new
	// required field after the first one.
	HasRegistrations(ctx context.Context, orgID, eventID uuid.UUID) (bool, error)

	// ListLintRules returns every form lint rule (02 §11.5; no org scope),
	// active or not, by class then creation.
	ListLintRules(ctx context.Context) ([]LintRule, error)
	// GetLintRule returns a rule or ErrNotFound.
	GetLintRule(ctx context.Context, id uuid.UUID) (LintRule, error)
	// InsertLintRule writes a new rule and returns it as stored.
	InsertLintRule(ctx context.Context, r LintRule) (LintRule, error)
	// UpdateLintRule writes class, pattern, description_tr, active and
	// updated_by of r and returns the row; ErrNotFound when it is gone.
	UpdateLintRule(ctx context.Context, r LintRule) (LintRule, error)

	// StaffRole returns userID's staff role on event id (of orgID).
	StaffRole(ctx context.Context, orgID, eventID, userID uuid.UUID) (StaffRole, bool, error)
	// ListStaff returns the staff of event id (of orgID), oldest first.
	ListStaff(ctx context.Context, orgID, eventID uuid.UUID) ([]Staff, error)
	// InsertStaff writes a staff row; org_id is the event's organiser (06
	// H-38). ErrNotFound when the event is not s.OrgID's; ErrStaffExists
	// when the user is already staff of the event.
	InsertStaff(ctx context.Context, s Staff) (Staff, error)
	// UpdateStaffRole changes the role of a staff member and returns the
	// row; ErrNotFound when the user is not staff of the event.
	UpdateStaffRole(ctx context.Context, orgID, eventID, userID uuid.UUID, role StaffRole) (Staff, error)
	// DeleteStaff removes a staff member; ErrNotFound when the user is not
	// staff of the event.
	DeleteStaff(ctx context.Context, orgID, eventID, userID uuid.UUID) error
	// StaffOfUser returns every staff row of a user, oldest first (KVKK).
	StaffOfUser(ctx context.Context, userID uuid.UUID) ([]Staff, error)
	// DeleteStaffOfUser deletes every staff row of a user and reports how
	// many it deleted (KVKK erasure).
	DeleteStaffOfUser(ctx context.Context, userID uuid.UUID) (int, error)
}

// Sort orders of ListEvents.
const (
	SortStartsAt     = "starts_at"     // soonest first (default)
	SortPublishedNew = "-published_at" // newest publication first
)

// Viewer is who lists events (03 §3.6): what makes a non-public event
// visible to them.
type Viewer struct {
	All          bool        // a platform role: every published event
	UniversityID *uuid.UUID  // a verified student's university: university events of it
	MemberOrgs   []uuid.UUID // active memberships: university and members events of these orgs
	UserID       *uuid.UUID  // staff of an event see it whatever its visibility
}

// ListFilter narrows ListEvents. Zero fields do not filter.
type ListFilter struct {
	Viewer Viewer
	// Q is free text (2–100 characters): full-text match on title, summary
	// and description (koro_tr), or a substring / trigram match of the
	// normalised title (koro_norm; BE-020's pattern, 05 §9.5).
	Q string
	// OrgIDs restricts the organisers when non-nil (an empty, non-nil
	// slice matches nothing): org_id or university_id of the request.
	OrgIDs   []uuid.UUID
	Category Category
	Format   Format
	City     string // koro_norm equality
	// StartsFrom / StartsTo bound starts_at (inclusive / exclusive).
	StartsFrom *time.Time
	StartsTo   *time.Time
	// EndsAfter hides events that ended at or before it (the service sets
	// it to now when no date range is given: upcoming and running events).
	EndsAfter *time.Time
	Sort      string // SortStartsAt (default) or SortPublishedNew
}

// OrgListFilter narrows ListOrgEvents. Empty Statuses lists every status.
type OrgListFilter struct {
	Statuses []Status
}

// After is the keyset position of a page: the last row's sort value and id.
type After struct {
	At time.Time // starts_at, or published_at for SortPublishedNew
	ID uuid.UUID
}
