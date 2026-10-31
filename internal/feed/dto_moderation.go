package feed

import (
	"time"
	"uuid"

	"koro/internal/outreach"
	"koro/internal/students"
)

// DTOs of BE-032 (04 §2.7, §F.1, §F.4).

// EntitlementLeftDTO is what a company has left of the right a targeted
// post consumed (04 §F.1).
type EntitlementLeftDTO struct {
	Feature   string `json:"feature" enum:"targeted_post"`
	Remaining int    `json:"remaining" doc:"Units left after this post"`
}

// TargetedPostDTO answers createTargetedPost: the draft in the management
// view, its published audience estimate (level 0: rounded down, no number
// below 10 or for a sensitive club) and, for a company, the targeted_post
// units left.
type TargetedPostDTO struct {
	PostManageDTO
	AudienceEstimate students.AudienceCountDTO `json:"audience_estimate"`
	Entitlement      *EntitlementLeftDTO       `json:"entitlement,omitempty"`
	DispatchID       string                    `json:"dispatch_id,omitempty" format:"uuid" doc:"The scheduled notification of the audience (getDispatch)"`
}

func toTargetedDTO(v TargetedView) TargetedPostDTO {
	d := TargetedPostDTO{
		PostManageDTO:    toManageDTO(v.PostView),
		AudienceEstimate: estimateDTO(v.Estimate),
	}
	if v.DispatchID != nil {
		d.DispatchID = v.DispatchID.String()
	}
	if v.Remaining != nil {
		d.Entitlement = &EntitlementLeftDTO{Feature: "targeted_post", Remaining: *v.Remaining}
	}
	return d
}

// ReportCreatedDTO answers createContentReport.
type ReportCreatedDTO struct {
	ReportID string `json:"report_id"`
}

// ReportedPostDTO is the reported post as a moderator sees it.
type ReportedPostDTO struct {
	PostID         string            `json:"post_id"`
	Kind           string            `json:"kind" enum:"text,image,event_share,opportunity_call"`
	Status         string            `json:"status" enum:"draft,pending_review,published,hidden,removed"`
	Title          string            `json:"title,omitempty"`
	BodyText       string            `json:"body_text,omitempty" doc:"Plain-text preview of the body"`
	AudienceKind   string            `json:"audience_kind" enum:"public,followers,segment"`
	CommercialFlag bool              `json:"commercial_flag"`
	PublishedAt    *time.Time        `json:"published_at,omitempty"`
	EditedAt       *time.Time        `json:"edited_at,omitempty" doc:"Last edit after the publication"`
	Media          []MediaDTO        `json:"media" doc:"The images the post shows now"`
	DroppedMedia   []DroppedMediaDTO `json:"dropped_media" doc:"Images edits took off the post after its publication (BE-158), newest last: kept for the moderators until 12 months after the edit, and while a report on the post is open; readers and the author organization no longer see them"`
}

// DroppedMediaDTO is an image an edit took off a published post (BE-158).
type DroppedMediaDTO struct {
	FileID    string    `json:"file_id"`
	URL       string    `json:"url,omitempty" doc:"Short-lived download URL (at most 5 minutes); absent when the file is not served"`
	AltText   string    `json:"alt_text,omitempty"`
	Width     *int      `json:"width,omitempty"`
	Height    *int      `json:"height,omitempty"`
	DroppedAt time.Time `json:"dropped_at" doc:"When the edit took it off the post"`
}

// ReportTargetDTO summarises the target of a report (adminGetReport).
type ReportTargetDTO struct {
	Type        string           `json:"type" enum:"post,org"`
	Org         FeedOrgDTO       `json:"org" doc:"The reported organization, or the post's author organization"`
	OrgStatus   string           `json:"org_status" enum:"pending_review,active,suspended,closed"`
	TrustLevel  string           `json:"trust_level" enum:"new,trusted,restricted,suspended" doc:"Feed trust level"`
	Post        *ReportedPostDTO `json:"post,omitempty"`
	OpenReports int              `json:"open_reports" doc:"Distinct users with an open report on the target"`
}

// AdminReportDTO is a report for the moderators (04 §2.7). The reporter's
// identity appears here only (03 Y7).
type AdminReportDTO struct {
	ReportID       string           `json:"report_id"`
	ReporterUserID string           `json:"reporter_user_id" doc:"Shown on the admin report endpoints only, never to the reported organization"`
	TargetType     string           `json:"target_type" enum:"post,org"`
	TargetID       string           `json:"target_id"`
	Reason         string           `json:"reason" enum:"spam,harassment,hate,sexual,scam_or_misleading_job,copyright,privacy,other"`
	Details        string           `json:"details,omitempty"`
	Status         string           `json:"status" enum:"open,in_review,actioned,dismissed"`
	AssignedTo     string           `json:"assigned_to,omitempty"`
	ResolutionNote string           `json:"resolution_note,omitempty"`
	ResolvedAt     *time.Time       `json:"resolved_at,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	Target         *ReportTargetDTO `json:"target,omitempty" doc:"adminGetReport and adminResolveReport only"`
}

// AdminReportList is a page of adminListReports.
type AdminReportList struct {
	Items      []AdminReportDTO `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

func optString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func idText(p *uuid.UUID) string {
	if p == nil {
		return ""
	}
	return p.String()
}

func toReportDTO(v ReportView) AdminReportDTO {
	r := v.Report
	d := AdminReportDTO{
		ReportID: r.ID.String(), ReporterUserID: r.ReporterUserID.String(), TargetType: string(r.TargetType),
		TargetID: r.TargetID.String(), Reason: string(r.Reason), Details: optString(r.Details), Status: string(r.Status),
		AssignedTo: idText(r.AssignedTo), ResolutionNote: optString(r.ResolutionNote), ResolvedAt: r.ResolvedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if t := v.Target; t != nil {
		o := t.Org
		td := &ReportTargetDTO{
			Type: string(t.Type), Org: FeedOrgDTO{OrgID: o.ID.String(), Kind: string(o.Kind), Name: o.Name, Slug: o.Slug, Verified: o.Verified},
			OrgStatus: string(o.Status), TrustLevel: string(t.TrustLevel), OpenReports: t.OpenReports,
		}
		if p := t.Post; p != nil {
			td.Post = &ReportedPostDTO{
				PostID: p.ID.String(), Kind: string(p.Kind), Status: string(p.Status), Title: optString(p.Title),
				BodyText: optString(p.BodyText), AudienceKind: string(p.AudienceKind), CommercialFlag: p.CommercialFlag,
				PublishedAt: p.PublishedAt, EditedAt: p.EditedAt, Media: []MediaDTO{}, DroppedMedia: []DroppedMediaDTO{},
			}
			for _, m := range t.Media {
				td.Post.Media = append(td.Post.Media, toMediaDTO(m))
			}
			for _, m := range t.DroppedMedia {
				td.Post.DroppedMedia = append(td.Post.DroppedMedia, DroppedMediaDTO{FileID: m.FileID.String(), URL: m.URL,
					AltText: optString(m.AltText), Width: m.Width, Height: m.Height, DroppedAt: m.DroppedAt})
			}
		}
		d.Target = td
	}
	return d
}

// ModerationActionDTO is one moderation decision (02 §12.10; append-only).
type ModerationActionDTO struct {
	ModerationActionID string    `json:"moderation_action_id"`
	ReportID           string    `json:"report_id,omitempty"`
	TargetType         string    `json:"target_type" enum:"post,org"`
	TargetID           string    `json:"target_id"`
	Action             string    `json:"action" enum:"approve,hide,remove,restore,warn_org,restrict_org,suspend_org"`
	ReasonCode         string    `json:"reason_code"`
	ActorUserID        string    `json:"actor_user_id" doc:"The moderator (platform staff)"`
	Note               string    `json:"note,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

func toActionDTO(m ModerationAction) ModerationActionDTO {
	return ModerationActionDTO{
		ModerationActionID: m.ID.String(), ReportID: idText(m.ReportID), TargetType: string(m.TargetType),
		TargetID: m.TargetID.String(), Action: string(m.Action), ReasonCode: m.ReasonCode,
		ActorUserID: m.ActorUserID.String(), Note: optString(m.Note), CreatedAt: m.CreatedAt,
	}
}

// ModerationActionList is a page of adminListModerationActions.
type ModerationActionList struct {
	Items      []ModerationActionDTO `json:"items"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

// PostModerationDTO answers adminModeratePost.
type PostModerationDTO struct {
	PostID           string              `json:"post_id"`
	Status           string              `json:"status" enum:"draft,pending_review,published,hidden,removed"`
	ModerationAction ModerationActionDTO `json:"moderation_action"`
}

// OrgModerationDTO answers adminModerateOrg: the action and the org's feed
// trust level after it.
type OrgModerationDTO struct {
	OrgID            string              `json:"org_id"`
	TrustLevel       string              `json:"trust_level" enum:"new,trusted,restricted,suspended"`
	RestrictedUntil  *time.Time          `json:"restricted_until,omitempty"`
	ModerationAction ModerationActionDTO `json:"moderation_action"`
}

// estimateDTO turns a published estimate into the level 0 DTO.
func estimateDTO(e outreach.Estimate) students.AudienceCountDTO {
	return students.AudienceCountDTO{Count: e.Count, BelowThreshold: e.BelowThreshold}
}
