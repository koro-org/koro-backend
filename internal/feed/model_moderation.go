package feed

import (
	"bytes"
	"time"
	"uuid"
)

// TargetType is content_reports.target_type and moderation_actions.target_type
// (02 §12.9–12.10): a post or an org; a report also targets a conversation
// message (BE-162b, 000174), never a moderation action.
type TargetType string

// Report and moderation targets. TargetMessage is a report's only: a party
// of a conversation reports a message of the other side through messaging
// (FileMessageReport), which keeps the reported messages; the measure a
// moderator takes is another module's (the company's or the user's
// suspension, the conversation's closing), so a message report is actioned
// with a resolution note instead of a moderation action.
const (
	TargetPost    TargetType = "post"
	TargetOrg     TargetType = "org"
	TargetMessage TargetType = "message"
)

// Valid reports whether t is a target type of moderation_actions (post,
// org).
func (t TargetType) Valid() bool { return t == TargetPost || t == TargetOrg }

// ReportValid reports whether t is a target type of content_reports (post,
// org, message; content_reports_target_type_chk).
func (t TargetType) ReportValid() bool { return t.Valid() || t == TargetMessage }

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
	PostSnapshot
	OrgSnapshot
	CreatedAt time.Time `gorm:"column:created_at;->"`
	UpdatedAt time.Time `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Report) TableName() string { return "content_reports" }

// PostSnapshot is the reported post as it stood when its report was filed
// (BE-160, 000168): written once with a post report, never changed
// (content_reports_snapshot_guard), so an edit of the post leaves the
// moderators what was reported. SnapshotAt, shared with OrgSnapshot, nil
// means no snapshot (a report filed before BE-160, or before BE-180 on an
// org); with it, a nil field means the post had none. The copy is the
// reported org's content: it goes with
// its report (the reporter's erasure, content_report retention) and is
// shown on the moderators' report view only (adminGetReport,
// adminResolveReport), never in the reporter's export.
type PostSnapshot struct {
	SnapshotTitle       *string    `gorm:"column:snapshot_title"`
	SnapshotBodyText    *string    `gorm:"column:snapshot_body_text"`
	SnapshotOpportunity JSON       `gorm:"column:snapshot_opportunity;type:jsonb"`
	SnapshotEventID     *uuid.UUID `gorm:"type:uuid;column:snapshot_event_id"`
	SnapshotAt          *time.Time `gorm:"column:snapshot_at"`
}

// snapshotOf is p's snapshot taken at at.
func snapshotOf(p Post, at time.Time) PostSnapshot {
	return PostSnapshot{SnapshotTitle: p.Title, SnapshotBodyText: p.BodyText, SnapshotOpportunity: bytes.Clone(p.Opportunity),
		SnapshotEventID: p.EventID, SnapshotAt: &at}
}

// clone deep-copies the snapshot's JSON (the in-memory store keeps rows
// apart from their callers).
func (s PostSnapshot) clone() PostSnapshot {
	s.SnapshotOpportunity = bytes.Clone(s.SnapshotOpportunity)
	return s
}

// OrgSnapshot is the reported org's profile as it stood when its report was
// filed (BE-180, 000180), under the rules of PostSnapshot: written once with
// an org report at PostSnapshot.SnapshotAt, never changed, shown to the
// moderators only, never in the reporter's export. The name is always there;
// another nil field means the org had none. SnapshotOrgLogoFileID names the
// file and outlives it: the file stays only while the org uses it or
// org_dropped_logos keeps it.
type OrgSnapshot struct {
	SnapshotOrgName        *string    `gorm:"column:snapshot_org_name"`
	SnapshotOrgDescription *string    `gorm:"column:snapshot_org_description"`
	SnapshotOrgWebsiteURL  *string    `gorm:"column:snapshot_org_website_url"`
	SnapshotOrgSocialLinks JSON       `gorm:"column:snapshot_org_social_links;type:jsonb"`
	SnapshotOrgLogoFileID  *uuid.UUID `gorm:"type:uuid;column:snapshot_org_logo_file_id"`
}

// clone deep-copies the report's snapshot.
func (r Report) clone() Report {
	r.PostSnapshot = r.PostSnapshot.clone()
	r.SnapshotOrgSocialLinks = bytes.Clone(r.SnapshotOrgSocialLinks)
	return r
}

// DroppedLogo is one org_dropped_logos row (BE-180): a logo updateOrg
// replaced or removed while a report on the org was open or in review. Its
// file stays, retired (no public copy, not attachable again), for the
// moderators only (adminGetReport, adminResolveReport); it goes with the file
// or through org_logo_dropped.
type DroppedLogo struct {
	FileID    uuid.UUID `gorm:"type:uuid;column:file_id;primaryKey"`
	OrgID     uuid.UUID `gorm:"type:uuid;column:org_id"`
	DroppedAt time.Time `gorm:"column:dropped_at"`
}

// TableName implements gorm's Tabler.
func (DroppedLogo) TableName() string { return "org_dropped_logos" }

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
