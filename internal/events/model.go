package events

import (
	"bytes"
	"database/sql/driver"
	"fmt"
	"time"
	"uuid"
)

// Status is events.status (02 §11.1).
type Status string

// Event statuses. A draft becomes published (publishEvent); a published
// event is cancelled (cancelEvent) or completed (attendance, BE-031). Only a
// draft may be deleted (the BEFORE DELETE trigger events_draft_delete_only).
const (
	StatusDraft     Status = "draft"
	StatusPublished Status = "published"
	StatusCancelled Status = "cancelled"
	StatusCompleted Status = "completed"
)

// Category is events.category (02 §11.1).
type Category string

// Categories of the CHECK constraint.
const (
	CategoryWorkshop     Category = "workshop"
	CategoryTalk         Category = "talk"
	CategoryCompetition  Category = "competition"
	CategoryCareer       Category = "career"
	CategorySocial       Category = "social"
	CategoryTrip         Category = "trip"
	CategorySports       Category = "sports"
	CategoryCulture      Category = "culture"
	CategoryVolunteering Category = "volunteering"
	CategoryOther        Category = "other"
)

// Categories lists every category (OpenAPI enums).
func Categories() []Category {
	return []Category{CategoryWorkshop, CategoryTalk, CategoryCompetition, CategoryCareer, CategorySocial, CategoryTrip,
		CategorySports, CategoryCulture, CategoryVolunteering, CategoryOther}
}

// Valid reports whether c is a category of the CHECK constraint.
func (c Category) Valid() bool {
	for _, x := range Categories() {
		if c == x {
			return true
		}
	}
	return false
}

// Format is events.format.
type Format string

// Formats. online and hybrid events need online_url to be published.
const (
	FormatInPerson Format = "in_person"
	FormatOnline   Format = "online"
	FormatHybrid   Format = "hybrid"
)

// Valid reports whether f is a format of the CHECK constraint.
func (f Format) Valid() bool { return f == FormatInPerson || f == FormatOnline || f == FormatHybrid }

// Visibility is events.visibility (03 §3.6).
type Visibility string

// Visibilities.
const (
	VisibilityPublic     Visibility = "public"     // everyone, anonymous included
	VisibilityUniversity Visibility = "university" // verified students of one university + members + staff
	VisibilityMembers    Visibility = "members"    // the organiser's active members + staff
)

// Valid reports whether v is a visibility of the CHECK constraint.
func (v Visibility) Valid() bool {
	return v == VisibilityPublic || v == VisibilityUniversity || v == VisibilityMembers
}

// RegistrationMode is events.registration_mode.
type RegistrationMode string

// Registration modes ("manual": the UI says "Apply"; BE-028).
const (
	ModeAuto   RegistrationMode = "auto"
	ModeManual RegistrationMode = "manual"
)

// Valid reports whether m is a mode of the CHECK constraint.
func (m RegistrationMode) Valid() bool { return m == ModeAuto || m == ModeManual }

// Event is one events row (02 §11.1). The generated search_vector column is
// not mapped. The row holds no personal data (02 §11.1): CreatedBy is a
// reference to an account.
type Event struct {
	ID                     uuid.UUID        `gorm:"type:uuid;column:id;primaryKey"`
	OrgID                  uuid.UUID        `gorm:"type:uuid;column:org_id"`
	CreatedBy              uuid.UUID        `gorm:"type:uuid;column:created_by"`
	Title                  string           `gorm:"column:title"`
	Slug                   string           `gorm:"column:slug"`
	Summary                *string          `gorm:"column:summary"`
	DescriptionJSON        JSON             `gorm:"column:description_json;type:jsonb"`
	DescriptionText        *string          `gorm:"column:description_text"`
	Category               Category         `gorm:"column:category"`
	Format                 Format           `gorm:"column:format"`
	LocationName           *string          `gorm:"column:location_name"`
	Address                *string          `gorm:"column:address"`
	City                   *string          `gorm:"column:city"`
	OnlineURL              *string          `gorm:"column:online_url"`
	StartsAt               time.Time        `gorm:"column:starts_at"`
	EndsAt                 time.Time        `gorm:"column:ends_at"`
	CoverFileID            *uuid.UUID       `gorm:"type:uuid;column:cover_file_id"`
	Visibility             Visibility       `gorm:"column:visibility"`
	VisibilityUniversityID *uuid.UUID       `gorm:"type:uuid;column:visibility_university_id"`
	Status                 Status           `gorm:"column:status"`
	PublishedAt            *time.Time       `gorm:"column:published_at"`
	CancelledAt            *time.Time       `gorm:"column:cancelled_at"`
	CancelReason           *string          `gorm:"column:cancel_reason"`
	RequiresRegistration   bool             `gorm:"column:requires_registration"`
	RegistrationMode       RegistrationMode `gorm:"column:registration_mode"`
	Capacity               *int             `gorm:"column:capacity"`
	ConfirmedCount         int              `gorm:"column:confirmed_count"`
	WaitlistEnabled        bool             `gorm:"column:waitlist_enabled"`
	RegistrationOpensAt    *time.Time       `gorm:"column:registration_opens_at"`
	RegistrationClosesAt   *time.Time       `gorm:"column:registration_closes_at"`
	CancelDeadlineAt       *time.Time       `gorm:"column:cancel_deadline_at"`
	CheckinEnabled         bool             `gorm:"column:checkin_enabled"`
	AttendanceFinalizedAt  *time.Time       `gorm:"column:attendance_finalized_at"`
	AttendanceFinalizedBy  *uuid.UUID       `gorm:"type:uuid;column:attendance_finalized_by"`
	IsSensitiveCategory    bool             `gorm:"column:is_sensitive_category"`
	CreatedAt              time.Time        `gorm:"column:created_at;->"`
	UpdatedAt              time.Time        `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Event) TableName() string { return "events" }

// StaffRole is event_staff.role (02 §11.2; 03 §1.1 event_staff).
type StaffRole string

// Staff roles: scanner (the default) checks attendees in; manager also
// voids check-ins and closes attendance (03 §5.6 Em).
const (
	StaffScanner StaffRole = "scanner"
	StaffManager StaffRole = "manager"
)

// Valid reports whether r is a role of the CHECK constraint.
func (r StaffRole) Valid() bool { return r == StaffScanner || r == StaffManager }

// Staff is one event_staff row (02 §11.2): personal data (who works the
// door of which event). org_id is the event's organiser, copied from the
// events row by the store (06 H-38).
type Staff struct {
	EventID   uuid.UUID `gorm:"type:uuid;column:event_id;primaryKey"`
	UserID    uuid.UUID `gorm:"type:uuid;column:user_id;primaryKey"`
	OrgID     uuid.UUID `gorm:"type:uuid;column:org_id"`
	Role      StaffRole `gorm:"column:role"`
	InvitedBy uuid.UUID `gorm:"type:uuid;column:invited_by"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (Staff) TableName() string { return "event_staff" }

// Form is the event_forms row of an event (02 §11.3): the editable draft
// schema and the pointer to the current published version (BE-026). The
// event core creates the empty draft with the event; org_id comes from the
// events row (06 H-38).
type Form struct {
	ID               uuid.UUID  `gorm:"type:uuid;column:id;primaryKey"`
	EventID          uuid.UUID  `gorm:"type:uuid;column:event_id"`
	OrgID            uuid.UUID  `gorm:"type:uuid;column:org_id"`
	DraftSchema      JSON       `gorm:"column:draft_schema;type:jsonb"`
	CurrentVersionID *uuid.UUID `gorm:"type:uuid;column:current_version_id"`
	UpdatedBy        *uuid.UUID `gorm:"type:uuid;column:updated_by"`
	CreatedAt        time.Time  `gorm:"column:created_at;->"`
	UpdatedAt        time.Time  `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Form) TableName() string { return "event_forms" }

// emptyFormSchema is the draft schema of a new event's form (the column
// default of 02 §11.3).
var emptyFormSchema = JSON(`{"schema_version":1,"fields":[]}`)

// FormVersion is one event_form_versions row (02 §11.4): a published form
// schema, immutable (app_rw has SELECT/INSERT only and koro_forbid_mutation
// blocks UPDATE/DELETE). A registration keeps the version it was validated
// against (event_registrations.form_version_id), so a later version never
// changes what an earlier registration means. form_id and event_id come
// from the event_forms row (06 H-38); the lint report is part of the row:
// blocked is always empty, has_sensitive_fields mirrors warn_special and
// needs the override columns (the CHECKs of 000057).
type FormVersion struct {
	ID                 uuid.UUID  `gorm:"type:uuid;column:id;primaryKey"`
	FormID             uuid.UUID  `gorm:"type:uuid;column:form_id"`
	EventID            uuid.UUID  `gorm:"type:uuid;column:event_id"`
	VersionNo          int        `gorm:"column:version_no"`
	Schema             JSON       `gorm:"column:schema;type:jsonb"`
	SchemaSHA256       []byte     `gorm:"column:schema_sha256"`
	FieldCount         int        `gorm:"column:field_count"`
	HasSensitiveFields bool       `gorm:"column:has_sensitive_fields"`
	LintReport         JSON       `gorm:"column:lint_report;type:jsonb"`
	LintOverrideBy     *uuid.UUID `gorm:"type:uuid;column:lint_override_by"`
	LintOverrideReason *string    `gorm:"column:lint_override_reason"`
	PublishedBy        uuid.UUID  `gorm:"type:uuid;column:published_by"`
	PublishedAt        time.Time  `gorm:"column:published_at"`
}

// TableName implements gorm's Tabler.
func (FormVersion) TableName() string { return "event_form_versions" }

// LintClass is form_lint_rules.class (02 §11.5; T-133: 02 wins over R-003).
type LintClass string

// Lint classes. A blocked field (ID, card or account numbers, passwords and
// the special categories 02 does not list) cannot be published, with no
// override; a warn_special field (health, disability, diet, religion) and a
// review_identifier field (passport, licence, tax or social security
// numbers) need a reasoned override (lint_override_reason).
const (
	LintBlocked          LintClass = "blocked"
	LintWarnSpecial      LintClass = "warn_special"
	LintReviewIdentifier LintClass = "review_identifier"
)

// Valid reports whether c is a class of the CHECK constraint.
func (c LintClass) Valid() bool {
	return c == LintBlocked || c == LintWarnSpecial || c == LintReviewIdentifier
}

// LintRule is one form_lint_rules row (02 §11.5): an RE2 pattern matched on
// the folded (koro_norm) texts of a form field. Rules are data (the DPO
// tunes them, events.lint_rule.manage); a rule is deactivated, never
// deleted. No org scope.
type LintRule struct {
	ID            uuid.UUID  `gorm:"type:uuid;column:id;primaryKey"`
	Class         LintClass  `gorm:"column:class"`
	Pattern       string     `gorm:"column:pattern"`
	DescriptionTR string     `gorm:"column:description_tr"`
	Active        bool       `gorm:"column:active"`
	UpdatedBy     *uuid.UUID `gorm:"type:uuid;column:updated_by"`
	CreatedAt     time.Time  `gorm:"column:created_at;->"`
	UpdatedAt     time.Time  `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (LintRule) TableName() string { return "form_lint_rules" }

// EventSummary is the cross-module view of an event (01 §4.10 Events.Get).
// Sensitive = is_sensitive_category OR the organiser is a sensitive club
// (02 §11.1): consumers (feed, catalog, talent) treat such an event like
// sensitive club data. The flag is never logged.
type EventSummary struct {
	ID         uuid.UUID
	OrgID      uuid.UUID
	Title      string
	Slug       string // the public URL identifier (BE-106: web revalidation)
	Status     Status
	StartsAt   time.Time
	EndsAt     time.Time
	Visibility Visibility
	Sensitive  bool
}

// JSON is a jsonb column; nil is SQL NULL. Scan copies the driver's bytes.
type JSON []byte

// Value implements driver.Valuer.
func (j JSON) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	return string(j), nil
}

// Scan implements sql.Scanner (jsonb arrives as text or bytes).
func (j *JSON) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = bytes.Clone(v)
	case string:
		*j = JSON(v)
	default:
		return fmt.Errorf("events: jsonb: unexpected %T", src)
	}
	return nil
}
