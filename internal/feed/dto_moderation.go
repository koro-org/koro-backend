package feed

import (
	"encoding/json"
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

// ReportedPostDTO is the reported post as a moderator sees it: as it is
// now, and as it was when the report was filed (ReportedSnapshot, BE-160).
type ReportedPostDTO struct {
	PostID           string               `json:"post_id"`
	Kind             string               `json:"kind" enum:"text,image,event_share,opportunity_call"`
	Status           string               `json:"status" enum:"draft,pending_review,published,hidden,removed"`
	Title            string               `json:"title,omitempty"`
	BodyText         string               `json:"body_text,omitempty" doc:"Plain-text preview of the body"`
	EventID          string               `json:"event_id,omitempty" format:"uuid" doc:"The event an event_share post shares now"`
	Opportunity      *OpportunityDTO      `json:"opportunity,omitempty" doc:"The opportunity of an opportunity_call post now"`
	AudienceKind     string               `json:"audience_kind" enum:"public,followers,segment"`
	CommercialFlag   bool                 `json:"commercial_flag"`
	PublishedAt      *time.Time           `json:"published_at,omitempty"`
	EditedAt         *time.Time           `json:"edited_at,omitempty" doc:"Last edit after the publication"`
	Media            []MediaDTO           `json:"media" doc:"The images the post shows now"`
	DroppedMedia     []DroppedMediaDTO    `json:"dropped_media" doc:"Images edits took off the post after its publication (BE-158), newest last: kept for the moderators until 12 months after the edit, and while a report on the post is open; readers and the author organization no longer see them"`
	ReportedSnapshot *ReportedSnapshotDTO `json:"reported_snapshot,omitempty" doc:"The post's text as it stood when this report was filed (BE-160); absent for a report filed before the snapshots were kept. An edited_at later than taken_at means the post changed since"`
}

// ReportedSnapshotDTO is the reported post's text when the report was
// filed (BE-160): written once with the report, never changed. An empty
// field means the post had none then.
type ReportedSnapshotDTO struct {
	Title       string          `json:"title,omitempty"`
	BodyText    string          `json:"body_text,omitempty" doc:"Plain text of the body"`
	EventID     string          `json:"event_id,omitempty" format:"uuid" doc:"The event the post shared"`
	Opportunity *OpportunityDTO `json:"opportunity,omitempty"`
	TakenAt     time.Time       `json:"taken_at" doc:"When the report was filed"`
}

// toSnapshotDTO is the DTO of r's snapshot; nil without one.
func toSnapshotDTO(r Report) *ReportedSnapshotDTO {
	if r.SnapshotAt == nil {
		return nil
	}
	return &ReportedSnapshotDTO{Title: optString(r.SnapshotTitle), BodyText: optString(r.SnapshotBodyText),
		EventID: idText(r.SnapshotEventID), Opportunity: toOpportunityDTO(r.SnapshotOpportunity), TakenAt: *r.SnapshotAt}
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

// ReportedOrgDTO is the reported org, or a reported post's author org, as a
// moderator sees it. The profile, the kept logos and the snapshot are an org
// target's only (BE-180).
type ReportedOrgDTO struct {
	FeedOrgDTO
	Description      string                  `json:"description,omitempty" doc:"An org target's description now"`
	WebsiteURL       string                  `json:"website_url,omitempty" doc:"An org target's website now"`
	SocialLinks      map[string]string       `json:"social_links,omitempty" doc:"A club target's social links now"`
	LogoFileID       string                  `json:"logo_file_id,omitempty" format:"uuid" doc:"An org target's logo file now"`
	DroppedLogos     []DroppedLogoDTO        `json:"dropped_logos,omitempty" doc:"Logos an org target replaced or removed while a report on it was open or in review, oldest first: kept for the moderators until 12 months after the change, and as long as a report filed before it is held; the organization and the public no longer see them"`
	ReportedSnapshot *ReportedOrgSnapshotDTO `json:"reported_snapshot,omitempty" doc:"An org target's profile as it stood when this report was filed; absent for a report filed before the snapshots were kept"`
}

// ReportedOrgSnapshotDTO is the reported org's profile when the report was
// filed (BE-180): written once with the report, never changed. An empty field
// means the org had none then.
type ReportedOrgSnapshotDTO struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	WebsiteURL  string            `json:"website_url,omitempty"`
	SocialLinks map[string]string `json:"social_links,omitempty" doc:"A club's social links"`
	LogoFileID  string            `json:"logo_file_id,omitempty" format:"uuid" doc:"The logo file the org showed"`
	LogoURL     string            `json:"logo_url,omitempty" doc:"Short-lived download URL of that logo (at most 5 minutes); absent when the file is not served or is gone: a logo replaced after every report on the org was resolved is deleted"`
	TakenAt     time.Time         `json:"taken_at" doc:"When the report was filed"`
}

// toOrgSnapshotDTO is the DTO of r's org snapshot; nil without one. logoURL
// is the signed URL of the logo it names.
func toOrgSnapshotDTO(r Report, logoURL string) *ReportedOrgSnapshotDTO {
	if r.SnapshotAt == nil || r.SnapshotOrgName == nil {
		return nil
	}
	return &ReportedOrgSnapshotDTO{Name: *r.SnapshotOrgName, Description: optString(r.SnapshotOrgDescription),
		WebsiteURL: optString(r.SnapshotOrgWebsiteURL), SocialLinks: decodeLinks(r.SnapshotOrgSocialLinks),
		LogoFileID: idText(r.SnapshotOrgLogoFileID), LogoURL: logoURL, TakenAt: *r.SnapshotAt}
}

// decodeLinks reads the social links of an org snapshot; nil without any.
func decodeLinks(j JSON) map[string]string {
	if j == nil {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(j, &m) != nil {
		return nil
	}
	return m
}

// DroppedLogoDTO is a logo an org replaced or removed while reported (BE-180).
type DroppedLogoDTO struct {
	FileID    string    `json:"file_id" format:"uuid"`
	URL       string    `json:"url,omitempty" doc:"Short-lived download URL (at most 5 minutes); absent when the file is not served"`
	DroppedAt time.Time `json:"dropped_at" doc:"When the organization replaced or removed it"`
}

// ReportTargetDTO summarises the target of a report (adminGetReport).
type ReportTargetDTO struct {
	Type        string           `json:"type" enum:"post,org"`
	Org         ReportedOrgDTO   `json:"org" doc:"The reported organization, or the post's author organization"`
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
	TargetType     string           `json:"target_type" enum:"post,org,message" doc:"message: a conversation message (BE-162b), reported by a party of its conversation; its messages are read with adminGetReportedMessages"`
	TargetID       string           `json:"target_id"`
	Reason         string           `json:"reason" enum:"spam,harassment,hate,sexual,scam_or_misleading_job,copyright,privacy,other"`
	Details        string           `json:"details,omitempty"`
	Status         string           `json:"status" enum:"open,in_review,actioned,dismissed"`
	AssignedTo     string           `json:"assigned_to,omitempty"`
	ResolutionNote string           `json:"resolution_note,omitempty"`
	ResolvedAt     *time.Time       `json:"resolved_at,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	Target         *ReportTargetDTO `json:"target,omitempty" doc:"adminGetReport and adminResolveReport only; absent for a message report (adminGetReportedMessages, with a justification)"`
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
			Type: string(t.Type), Org: ReportedOrgDTO{FeedOrgDTO: FeedOrgDTO{OrgID: o.ID.String(), Kind: string(o.Kind), Name: o.Name, Slug: o.Slug, Verified: o.Verified}},
			OrgStatus: string(o.Status), TrustLevel: string(t.TrustLevel), OpenReports: t.OpenReports,
		}
		if t.Type == TargetOrg {
			td.Org.ReportedSnapshot = toOrgSnapshotDTO(r, t.SnapshotLogoURL)
			for _, l := range t.DroppedLogos {
				td.Org.DroppedLogos = append(td.Org.DroppedLogos, DroppedLogoDTO{FileID: l.FileID.String(), URL: l.URL, DroppedAt: l.DroppedAt})
			}
			if p := t.Profile; p != nil {
				td.Org.LogoURL, td.Org.Description, td.Org.WebsiteURL = p.LogoURL, optString(p.Description), optString(p.WebsiteURL)
				td.Org.SocialLinks, td.Org.LogoFileID = p.SocialLinks, idText(p.LogoFileID)
			}
		}
		if p := t.Post; p != nil {
			td.Post = &ReportedPostDTO{
				PostID: p.ID.String(), Kind: string(p.Kind), Status: string(p.Status), Title: optString(p.Title),
				BodyText: optString(p.BodyText), EventID: idText(p.EventID), Opportunity: toOpportunityDTO(p.Opportunity),
				AudienceKind: string(p.AudienceKind), CommercialFlag: p.CommercialFlag, PublishedAt: p.PublishedAt,
				EditedAt: p.EditedAt, Media: []MediaDTO{}, DroppedMedia: []DroppedMediaDTO{}, ReportedSnapshot: toSnapshotDTO(r),
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
