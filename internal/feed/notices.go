package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// Moderation notices (BE-032; 01 §6.2, T-249). All are in-app,
// transactional, preference "moderation"; their texts are fixed per
// template and their data are opaque IDs (notify never renders them).
//
//	post.submitted_for_review → moderators: a post waits in the review queue
//	content_report.created    → moderators: a new report
//	post.moderated            → the author org's writers: the decision and the way to appeal
//	feed.notify_org_moderated → the org's writers: a warning, restriction or suspension
//	feed.notify_report_resolved → the reporter: the report was handled (R-003 §1.5.3)
//
// Moderators are the moderator role holders in effect; without any, the
// platform admins (the queue is never silent). Every send carries a dedupe
// key per event and recipient, so a retried task notifies once.

// Task names of the moderation notices.
const (
	// NotifyReviewQueueTaskName consumes post.submitted_for_review (T-249).
	NotifyReviewQueueTaskName = "feed.notify_review_queue"
	// NotifyReportCreatedTaskName consumes content_report.created.
	NotifyReportCreatedTaskName = "feed.notify_report_created"
	// NotifyPostModeratedTaskName consumes post.moderated.
	NotifyPostModeratedTaskName = "feed.notify_post_moderated"
	// NotifyOrgModeratedTaskName is the notice of an org decision (no
	// topic in 01 §6.2: a plain task, mechanism A).
	NotifyOrgModeratedTaskName = "feed.notify_org_moderated"
	// NotifyReportResolvedTaskName tells the reporter their report was
	// handled (a plain task, mechanism A).
	NotifyReportResolvedTaskName = "feed.notify_report_resolved"
)

// Templates of the moderation notices (internal/notify/templates/feed).
const (
	templateReviewSubmitted = "feed.review_submitted"
	templateReportCreated   = "feed.report_created"
	templatePostApproved    = "feed.post_approved"
	templatePostActioned    = "feed.post_actioned" // hidden or removed: reason code in the management list, appeal
	templatePostRestored    = "feed.post_restored"
	templateOrgModerated    = "feed.org_moderated"
	templateReportResolved  = "feed.report_resolved"
)

// orgModeratedTask is the payload of feed.notify_org_moderated.
type orgModeratedTask struct {
	ActionID uuid.UUID `json:"action_id"`
	OrgID    uuid.UUID `json:"org_id"`
	Action   string    `json:"action"`
}

// reportResolvedTask is the payload of feed.notify_report_resolved.
type reportResolvedTask struct {
	ReportID       uuid.UUID `json:"report_id"`
	ReporterUserID uuid.UUID `json:"reporter_user_id"`
}

// Tasks returns the plain task handlers of feed (no topic).
func (m Module) Tasks() []queue.Task {
	return []queue.Task{
		{Name: NotifyOrgModeratedTaskName, Handle: m.svc.handleOrgModerated},
		{Name: NotifyReportResolvedTaskName, Handle: m.svc.handleReportResolved},
	}
}

// notice sends one in-app moderation notice. A permanent failure (an
// account that cannot receive it) is logged and skipped; anything else is
// returned so the task is retried.
func (s *service) notice(ctx context.Context, to uuid.UUID, tmpl, link string, org *uuid.UUID, data map[string]any, dedupe string) error {
	if s.d.Sender == nil {
		return errors.New("feed: notify is not wired")
	}
	_, err := s.d.Sender.Send(ctx, notify.Message{
		UserID: &to, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmpl,
		SourceOrgID: org, Data: data, LinkPath: link, DedupeKey: dedupe,
	})
	if err != nil && notify.IsPermanent(err) {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "moderation notice not deliverable", slog.String("template", tmpl),
			slog.String("user_id", to.String()), slog.String("err", err.Error()))
		return nil
	}
	return err
}

// moderators returns who works the moderation queue: the moderators in
// effect, or the platform admins when there is none.
func (s *service) moderators(ctx context.Context) ([]uuid.UUID, error) {
	if s.d.Staff == nil {
		return nil, errors.New("feed: the moderator lookup is not wired")
	}
	ids, err := s.d.Staff.Moderators(ctx)
	if err != nil || len(ids) > 0 {
		return ids, err
	}
	return s.d.Staff.PlatformAdmins(ctx)
}

// writersOf returns the active members of orgID who write its posts (the
// recipients of the org's moderation notices).
func (s *service) writersOf(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	members, err := s.d.Orgs.ActiveMemberIDs(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var out []uuid.UUID
	for _, m := range members {
		ok, err := s.d.Orgs.HasRole(ctx, orgID, m, writerRoles...)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// handleSubmittedForReview is feed.notify_review_queue
// (post.submitted_for_review): every moderator is told a post waits.
func (s *service) handleSubmittedForReview(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PostSubmittedForReview](e)
	if err != nil {
		return queue.Permanent(err)
	}
	to, err := s.moderators(ctx)
	if err != nil {
		return err
	}
	org := p.OrgID
	for _, u := range to {
		if err := s.notice(ctx, u, templateReviewSubmitted, "/admin/review-queue", &org,
			map[string]any{"post_id": p.PostID, "org_id": p.OrgID},
			"feed.review_submitted:"+p.PostID.String()+":"+u.String()); err != nil {
			return err
		}
	}
	return nil
}

// handleReportCreated is feed.notify_report_created
// (content_report.created): every moderator is told of the new report.
func (s *service) handleReportCreated(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ContentReportCreated](e)
	if err != nil {
		return queue.Permanent(err)
	}
	to, err := s.moderators(ctx)
	if err != nil {
		return err
	}
	for _, u := range to {
		if err := s.notice(ctx, u, templateReportCreated, "/admin/reports/"+p.ReportID.String(), nil,
			map[string]any{"report_id": p.ReportID},
			"feed.report_created:"+p.ReportID.String()+":"+u.String()); err != nil {
			return err
		}
	}
	return nil
}

// postTemplates maps a post decision to its notice.
var postTemplates = map[ModAction]string{
	ModApprove: templatePostApproved, ModHide: templatePostActioned, ModRemove: templatePostActioned,
	ModRestore: templatePostRestored,
}

// handlePostModerated is feed.notify_post_moderated (post.moderated): the
// author org's writers learn the decision; the reason code is in their
// management list (listOrgPostsManage), the notice says how to appeal.
func (s *service) handlePostModerated(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PostModerated](e)
	if err != nil {
		return queue.Permanent(err)
	}
	tmpl, ok := postTemplates[ModAction(p.Action)]
	if !ok {
		return queue.Permanent(fmt.Errorf("feed: post.moderated with action %q", p.Action))
	}
	to, err := s.writersOf(ctx, p.OrgID)
	if err != nil {
		return err
	}
	org := p.OrgID
	for _, u := range to {
		if err := s.notice(ctx, u, tmpl, "/orgs/"+p.OrgID.String()+"/posts", &org,
			map[string]any{"post_id": p.PostID, "org_id": p.OrgID},
			"feed.post_moderated:"+e.ID.String()+":"+u.String()); err != nil {
			return err
		}
	}
	return nil
}

// handleOrgModerated is feed.notify_org_moderated: the org's writers learn
// of a warning, restriction or suspension and how to appeal.
func (s *service) handleOrgModerated(ctx context.Context, payload []byte) error {
	var t orgModeratedTask
	if err := json.Unmarshal(payload, &t); err != nil || t.ActionID == uuid.Nil() || t.OrgID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("feed: bad org moderation notice payload: %w", errors.Join(err, errors.New("ids required"))))
	}
	to, err := s.writersOf(ctx, t.OrgID)
	if err != nil {
		return err
	}
	org := t.OrgID
	for _, u := range to {
		if err := s.notice(ctx, u, templateOrgModerated, "/orgs/"+t.OrgID.String()+"/posts", &org,
			map[string]any{"org_id": t.OrgID, "moderation_action_id": t.ActionID},
			"feed.org_moderated:"+t.ActionID.String()+":"+u.String()); err != nil {
			return err
		}
	}
	return nil
}

// handleReportResolved is feed.notify_report_resolved: the reporter learns
// their report was handled (not how: the outcome may concern others).
func (s *service) handleReportResolved(ctx context.Context, payload []byte) error {
	var t reportResolvedTask
	if err := json.Unmarshal(payload, &t); err != nil || t.ReportID == uuid.Nil() || t.ReporterUserID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("feed: bad report notice payload: %w", errors.Join(err, errors.New("ids required"))))
	}
	return s.notice(ctx, t.ReporterUserID, templateReportResolved, "", nil, map[string]any{"report_id": t.ReportID},
		"feed.report_resolved:"+t.ReportID.String())
}
