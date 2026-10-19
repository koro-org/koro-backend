package feed

import (
	"time"
	"uuid"
)

// TargetType is content_reports.target_type and moderation_actions.target_type
// (02 §12.9–12.10): a post or an org.
type TargetType string

// Report and moderation targets.
const (
	TargetPost TargetType = "post"
	TargetOrg  TargetType = "org"
)

// Valid reports whether t is a target type of the CHECK constraints.
func (t TargetType) Valid() bool { return t == TargetPost || t == TargetOrg }

// ReportReason is content_reports.reason (02 §12.9; R-003 §1.5.3).
type ReportReason string

// Report reasons.
const (
	ReasonSpam             ReportReason = "spam"
	ReasonHarassment       ReportReason = "harassment"
	ReasonHate             ReportReason = "hate"
	ReasonSexual           ReportReason = "sexual"
	ReasonScamOrMisleading ReportReason = "scam_or_misleading_job"
	ReasonCopyright        ReportReason = "copyright"
	ReasonPrivacy          ReportReason = "privacy"
	ReasonOther            ReportReason = "other"
)

// Valid reports whether r is a reason of the CHECK constraint.
func (r ReportReason) Valid() bool {
	switch r {
	case ReasonSpam, ReasonHarassment, ReasonHate, ReasonSexual, ReasonScamOrMisleading, ReasonCopyright,
		ReasonPrivacy, ReasonOther:
		return true
	}
	return false
}

// ReportStatus is content_reports.status (02 §12.9): open → in_review (a
// moderator took it) → actioned (a moderation action was taken) or
// dismissed. open and in_review are "open" for the one-open-report rule
// (content_reports_open_uq) and the auto-hide threshold.
type ReportStatus string

// Report statuses.
const (
	ReportOpen      ReportStatus = "open"
	ReportInReview  ReportStatus = "in_review"
	ReportActioned  ReportStatus = "actioned"
	ReportDismissed ReportStatus = "dismissed"
)

// Valid reports whether s is a status of the CHECK constraint.
func (s ReportStatus) Valid() bool {
	return s == ReportOpen || s == ReportInReview || s == ReportActioned || s == ReportDismissed
}

// Unresolved reports whether the report still waits for a decision.
func (s ReportStatus) Unresolved() bool { return s == ReportOpen || s == ReportInReview }

// Report is one content_reports row (02 §12.9). ReporterUserID is personal
// data: it leaves the module only through the admin report endpoints (03
// Y7) and the reporter's KVKK export.
type Report struct {
	ID             uuid.UUID    `gorm:"type:uuid;column:id;primaryKey"`
	ReporterUserID uuid.UUID    `gorm:"type:uuid;column:reporter_user_id"`
	TargetType     TargetType   `gorm:"column:target_type"`
	TargetID       uuid.UUID    `gorm:"type:uuid;column:target_id"`
	Reason         ReportReason `gorm:"column:reason"`
	Details        *string      `gorm:"column:details"`
	Status         ReportStatus `gorm:"column:status"`
	AssignedTo     *uuid.UUID   `gorm:"type:uuid;column:assigned_to"`
	ResolutionNote *string      `gorm:"column:resolution_note"`
	ResolvedAt     *time.Time   `gorm:"column:resolved_at"`
	CreatedAt      time.Time    `gorm:"column:created_at;->"`
	UpdatedAt      time.Time    `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Report) TableName() string { return "content_reports" }

// ModAction is moderation_actions.action (02 §12.10).
type ModAction string

// Moderation actions: approve, hide, remove and restore act on a post;
// warn_org, restrict_org and suspend_org on an org (its feed trust level).
const (
	ModApprove     ModAction = "approve"
	ModHide        ModAction = "hide"
	ModRemove      ModAction = "remove"
	ModRestore     ModAction = "restore"
	ModWarnOrg     ModAction = "warn_org"
	ModRestrictOrg ModAction = "restrict_org"
	ModSuspendOrg  ModAction = "suspend_org"
)

// postAction reports whether a is a post action.
func (a ModAction) postAction() bool {
	return a == ModApprove || a == ModHide || a == ModRemove || a == ModRestore
}

// orgAction reports whether a is an org action.
func (a ModAction) orgAction() bool {
	return a == ModWarnOrg || a == ModRestrictOrg || a == ModSuspendOrg
}

// ModerationAction is one moderation_actions row (02 §12.10): append-only
// (the table's trigger refuses UPDATE and DELETE; only report_id is cleared
// when its report is purged). ActorUserID is the moderator (staff data).
type ModerationAction struct {
	ID          uuid.UUID  `gorm:"type:uuid;column:id;primaryKey"`
	ReportID    *uuid.UUID `gorm:"type:uuid;column:report_id"`
	TargetType  TargetType `gorm:"column:target_type"`
	TargetID    uuid.UUID  `gorm:"type:uuid;column:target_id"`
	Action      ModAction  `gorm:"column:action"`
	ReasonCode  string     `gorm:"column:reason_code"`
	ActorUserID uuid.UUID  `gorm:"type:uuid;column:actor_user_id"`
	Note        *string    `gorm:"column:note"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (ModerationAction) TableName() string { return "moderation_actions" }
