package feed

import (
	"context"
	"errors"
	"log/slog"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Reports (BE-032; 02 §12.9, 03 §5.7, 04 §2.7, §F.4; Apple 1.2, Google Play
// UGC): any signed-in user reports a post or an org they can see; the
// platform's moderators work the queue. The reporter's identity is shown
// on the admin endpoints only (03 Y7): the audit record of a report has no
// org scope, so it never appears in the reported org's audit trail.

// Report limits (02 §12.9).
const maxReportText = 1000

// autoHideReason is the reason code of a post hidden by the report
// threshold (no moderator: the audit record is the system's).
const autoHideReason = "reported_by_users"

// autoHideJob names the system actor of the automatic hide in the audit
// trail.
const autoHideJob = "feed.report_threshold"

// ReportInput is createContentReport's body (04 §2.7).
type ReportInput struct {
	TargetType TargetType
	TargetID   uuid.UUID
	Reason     ReportReason
	Details    string
}

// ReportResult answers createContentReport: Created is false when the
// reporter's open report on the target is returned (200, 04 §F.4).
type ReportResult struct {
	ID      uuid.UUID `json:"id"`
	Created bool      `json:"created"`
}

// ReportQuery narrows adminListReports.
type ReportQuery struct {
	Filter ReportFilter
	Limit  int
	After  *After
}

// ReportPage is a page of reports; Next is the keyset position
// (created_at, id) of the next page.
type ReportPage struct {
	Items []ReportView
	Next  *After
}

// ReportView is a report for the moderators. Target summarises what was
// reported (adminGetReport only; nil in lists and when the target is gone).
type ReportView struct {
	Report Report
	Target *TargetSummary
}

// TargetSummary is the reported post or org as a moderator sees it. A
// post target carries its images and the ones edits took off it since it
// was published (BE-158: what was reported stays in view), each with a
// 5-minute URL while the scan clears it. An org target carries its profile
// and the logos it replaced while reported (BE-180), the same way.
type TargetSummary struct {
	Type            TargetType
	Org             orgs.OrgSummary
	Post            *Post              // a post target
	Media           []MediaView        // the post's images
	DroppedMedia    []DroppedMediaView // the images edits took off the post (BE-158)
	Profile         *OrgProfileView    // an org target: its profile now (BE-180)
	DroppedLogos    []DroppedLogoView  // an org target: the logos kept for the moderators (BE-180)
	SnapshotLogoURL string             // an org target: the URL of the logo the report's snapshot names, if still stored
	TrustLevel      TrustLevel         // the author org's (or the org's) feed trust level
	OpenReports     int                // distinct users with an open report on the target
}

// DroppedMediaView is an image an edit took off a published post, for the
// moderators (BE-158).
type DroppedMediaView struct {
	DroppedMedia
	URL string
}

// ResolveInput is adminResolveReport's body (04 §2.7).
type ResolveInput struct {
	Status     ReportStatus // in_review, actioned or dismissed
	Note       string       // resolution_note; "" keeps the current one
	AssignToMe bool
}

// cleanText normalises an optional free text of at most max characters
// (05 §9.5): control and bidi characters are refused; "" is nil.
func cleanText(loc, s string, maxRunes int) (*string, error) {
	n, err := textx.NormalizeText(s)
	switch {
	case err != nil:
		return nil, problem.Field(loc, "contains control or bidi characters")
	case n == "":
		return nil, nil
	case utf8.RuneCountInString(n) > maxRunes:
		return nil, problem.Field(loc, "is too long")
	}
	return &n, nil
}

// reportTarget reads and authorises the target of a new report: a
// published post the reporter sees (an invisible one is 404, a draft its
// writers see 409) or an org whose profile the reporter sees.
func (s *service) reportTarget(ctx context.Context, a authz.Actor, t TargetType, id uuid.UUID) (*Post, error) {
	switch t {
	case TargetPost:
		p, err := s.posts().GetPost(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := s.authorize(ctx, a, authz.FeedReportCreate, p); err != nil {
			return nil, err
		}
		if p.Status != StatusPublished {
			return nil, ErrInvalidState
		}
		return &p, nil
	case TargetOrg:
		o, err := s.org(ctx, id)
		if err != nil {
			return nil, err
		}
		return nil, authz.Can(ctx, a, authz.FeedReportCreate, authz.OrgResource(o.ID, string(o.Kind), string(o.Status)))
	}
	return nil, problem.Field("body.target_type", "must be post or org")
}

// CreateReport implements Service. Order: the input (422) → the target
// (404/403/409) → in one transaction: the report or the reporter's open
// one (content_reports_open_uq), and for a new one its audit record,
// content_report.created (A) and the automatic hide of a post reported by
// enough distinct users. A new post report carries the snapshot of the
// post read for the target (BE-160): what the reporter reported stays for
// the moderators when the author edits the post; the reporter's open
// report returned again keeps its first snapshot. A new org report does the
// same with the org's profile, read in the transaction under a share lock
// (BE-180): updateOrg sees the report before it deletes a logo it names.
func (s *service) CreateReport(ctx context.Context, a authz.Actor, in ReportInput) (ReportResult, error) {
	if !a.IsUser() {
		return ReportResult{}, authz.ErrForbidden
	}
	details, err := reportText(in.Reason, in.Details)
	if err != nil {
		return ReportResult{}, err
	}
	post, err := s.reportTarget(ctx, a, in.TargetType, in.TargetID)
	if err != nil {
		return ReportResult{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op string
		In ReportInput
	}{"report", in})
	if err != nil {
		return ReportResult{}, err
	}
	var snap PostSnapshot
	if post != nil {
		snap = snapshotOf(*post, s.now())
	}
	res, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (ReportResult, error) {
		report := Report{
			ID: uuid.NewV7(), ReporterUserID: a.UserID, TargetType: in.TargetType, TargetID: in.TargetID,
			Reason: in.Reason, Details: details, PostSnapshot: snap,
		}
		if in.TargetType == TargetOrg {
			if err := s.takeOrgSnapshot(ctx, &report); err != nil {
				return ReportResult{}, err
			}
		}
		r, created, err := s.fileReport(ctx, a, report)
		if err != nil || !created {
			return ReportResult{ID: r.ID}, err
		}
		if post != nil {
			if err := s.autoHide(ctx, post.ID); err != nil {
				return ReportResult{}, err
			}
		}
		return ReportResult{ID: r.ID, Created: true}, nil
	})
	return res, err
}

// reportText checks a report's reason and cleans its details (422).
func reportText(reason ReportReason, details string) (*string, error) {
	if !reason.Valid() {
		return nil, problem.Field("body.reason", "is not a report reason")
	}
	return cleanText("body.details", details, maxReportText)
}

// fileReport writes r in the transaction carried by ctx: the new report or
// the reporter's open one on the target (content_reports_open_uq;
// created=false, nothing else written), and for a new one its audit record
// (no org scope: the reported side never sees who reported) and
// content_report.created (A: the moderators' notice).
func (s *service) fileReport(ctx context.Context, a authz.Actor, r Report) (Report, bool, error) {
	r, created, err := s.reports().InsertReport(ctx, r)
	if err != nil || !created {
		return r, created, err
	}
	id := r.ID
	if err := s.d.Audit.Log(ctx, audit.Event{
		Action: string(authz.FeedReportCreate), Outcome: audit.Success, ResourceType: reportAggType, ResourceID: &id,
		Metadata: map[string]any{"target_type": string(r.TargetType), "target_id": r.TargetID.String(),
			"reason": string(r.Reason)},
	}.By(a)); err != nil {
		return Report{}, false, err
	}
	s.emitFor(ctx, domainevent.ContentReportCreated{ReportID: r.ID, TargetType: string(r.TargetType)}, reportAggType, r.ID)
	return r, true, nil
}

// MessageReportInput is a report on a conversation message (BE-162b, Q-43):
// messaging files it for a party of the conversation (reportMyConversationMessage,
// reportOrgConversationMessage).
type MessageReportInput struct {
	MessageID uuid.UUID
	Reason    ReportReason
	Details   string
}

// MessageReports is the moderation queue's entry for conversation messages
// (BE-162b, Q-43; 01 §6.3 "Mesaj şikâyeti" doc patch): messaging authorises
// the reporter on the message's conversation (messaging.message.report) and
// keeps the reported messages in its own tables in the same transaction;
// feed keeps the report itself, so a message report is worked like any
// other (adminListReports, adminResolveReport) and goes like any other (the
// reporter's erasure, content_report retention).
type MessageReports interface {
	// FileMessageReport writes a's report of the message in the transaction
	// carried by ctx (no Idempotency-Key of its own: the caller's). A user
	// has one open report per message: their open report is returned again
	// with Created false, and nothing else is written. The reason and the
	// details are checked here (422 body.reason, body.details). The caller
	// authorised a on the message: the actor only signs the audit record.
	FileMessageReport(ctx context.Context, a authz.Actor, in MessageReportInput) (ReportResult, error)
}

// FileMessageReport implements MessageReports. A message report is never
// hidden automatically (a message is no feed content).
func (s *service) FileMessageReport(ctx context.Context, a authz.Actor, in MessageReportInput) (ReportResult, error) {
	if !a.IsUser() {
		return ReportResult{}, authz.ErrForbidden
	}
	details, err := reportText(in.Reason, in.Details)
	if err != nil {
		return ReportResult{}, err
	}
	r, created, err := s.fileReport(ctx, a, Report{
		ID: uuid.NewV7(), ReporterUserID: a.UserID, TargetType: TargetMessage, TargetID: in.MessageID,
		Reason: in.Reason, Details: details,
	})
	if err != nil {
		return ReportResult{}, err
	}
	return ReportResult{ID: r.ID, Created: created}, nil
}

// autoHide hides a published post once DefaultReportHideThreshold (or
// FEED_REPORT_HIDE_THRESHOLD) distinct users have an open report on it
// (R-003 §1.5.3): it waits hidden for a moderator, who restores or
// removes it. The author org is told (post.moderated, action hide).
func (s *service) autoHide(ctx context.Context, postID uuid.UUID) error {
	n, err := s.reports().OpenReporters(ctx, TargetPost, postID)
	if err != nil || n < s.d.ReportHideThreshold {
		return err
	}
	p, err := s.posts().LockPost(ctx, postID)
	if err != nil || p.Status != StatusPublished {
		return err
	}
	next := p
	next.Status = StatusHidden
	if _, err := s.posts().UpdatePost(ctx, next); err != nil {
		return err
	}
	if err := s.changed(ctx, p, next); err != nil {
		return err
	}
	id := p.ID
	sys := authz.SystemActor(autoHideJob)
	if err := s.d.Audit.Log(ctx, audit.Event{
		Action: "feed.post.auto_hide", Outcome: audit.Success, ResourceType: postAggType, ResourceID: &id,
		Metadata: map[string]any{"org_id": p.OrgID.String(), "open_reports": n, "threshold": s.d.ReportHideThreshold},
	}.By(sys)); err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "post hidden by the report threshold", slog.String("post_id", id.String()))
	s.emit(ctx, domainevent.PostModerated{PostID: p.ID, OrgID: p.OrgID, Action: string(ModHide),
		ReasonCode: autoHideReason}, p.ID)
	return nil
}

// ListReports implements Service (feed.report.list).
func (s *service) ListReports(ctx context.Context, a authz.Actor, q ReportQuery) (ReportPage, error) {
	if err := authz.Can(ctx, a, authz.FeedReportList, adminSurface); err != nil {
		return ReportPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.reports().ListReports(ctx, q.Filter, q.After, limit+1)
	if err != nil {
		return ReportPage{}, err
	}
	out := ReportPage{Items: []ReportView{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		out.Next = &After{At: last.CreatedAt, ID: last.ID}
	}
	for _, r := range rows {
		out.Items = append(out.Items, ReportView{Report: r})
	}
	return out, nil
}

// GetReport implements Service (feed.report.read).
func (s *service) GetReport(ctx context.Context, a authz.Actor, id uuid.UUID) (ReportView, error) {
	if err := authz.Can(ctx, a, authz.FeedReportRead, adminSurface); err != nil {
		return ReportView{}, err
	}
	r, err := s.reports().GetReport(ctx, id)
	if err != nil {
		return ReportView{}, err
	}
	return s.reportView(ctx, r)
}

// reportView adds the summary of the report's target. A message report has
// none here: its conversation, company and messages are messaging's
// (adminGetReportedMessages, each read with a justification; Q-43).
func (s *service) reportView(ctx context.Context, r Report) (ReportView, error) {
	if r.TargetType == TargetMessage {
		return ReportView{Report: r}, nil
	}
	t := &TargetSummary{Type: r.TargetType}
	orgID := r.TargetID
	if r.TargetType == TargetPost {
		p, err := s.posts().GetPost(ctx, r.TargetID)
		switch {
		case errors.Is(err, ErrNotFound):
			return ReportView{Report: r}, nil // a draft deleted since: nothing to show
		case err != nil:
			return ReportView{}, err
		}
		t.Post, orgID = &p, p.OrgID
		if err := s.reportedMedia(ctx, t); err != nil {
			return ReportView{}, err
		}
	}
	o, err := s.d.Orgs.Get(ctx, orgID)
	switch {
	case errors.Is(err, orgs.ErrNotFound):
		return ReportView{Report: r}, nil
	case err != nil:
		return ReportView{}, err
	}
	t.Org, t.TrustLevel = o, TrustNew
	if o.Kind == orgs.KindPlatform {
		t.TrustLevel = TrustTrusted
	} else if st, ok, err := s.settings().OrgSettings(ctx, o.ID); err != nil {
		return ReportView{}, err
	} else if ok {
		t.TrustLevel = st.TrustLevel
	}
	if t.OpenReports, err = s.reports().OpenReporters(ctx, r.TargetType, r.TargetID); err != nil {
		return ReportView{}, err
	}
	if r.TargetType == TargetOrg {
		if err := s.reportedOrg(ctx, t, r); err != nil {
			return ReportView{}, err
		}
	}
	return ReportView{Report: r, Target: t}, nil
}

// reportedMedia adds the images of the reported post t.Post: the ones it
// shows and the ones edits dropped (BE-158), with their URLs (a file the
// scan has not cleared, or none without the files module, has none).
func (s *service) reportedMedia(ctx context.Context, t *TargetSummary) error {
	ids := []uuid.UUID{t.Post.ID}
	media, err := s.media().MediaOf(ctx, ids)
	if err != nil {
		return err
	}
	dropped, err := s.media().DroppedMediaOf(ctx, ids)
	if err != nil {
		return err
	}
	for _, m := range media[t.Post.ID] {
		t.Media = append(t.Media, MediaView{Media: m, URL: s.mediaURL(ctx, m.FileID)})
	}
	for _, m := range dropped[t.Post.ID] {
		t.DroppedMedia = append(t.DroppedMedia, DroppedMediaView{DroppedMedia: m, URL: s.mediaURL(ctx, m.FileID)})
	}
	return nil
}

// ResolveReport implements Service (feed.report.resolve): open → in_review
// (taken, optionally assigned to the caller), open or in_review →
// actioned (a moderation action was taken with the report) or dismissed.
// A resolved report takes no other change (409); repeating its outcome is
// a no-op. Nobody resolves their own report (403). The reporter is told
// the report was handled (in-app, without the outcome's details).
func (s *service) ResolveReport(ctx context.Context, a authz.Actor, id uuid.UUID, in ResolveInput) (ReportView, error) {
	if err := authz.Can(ctx, a, authz.FeedReportResolve, adminSurface); err != nil {
		return ReportView{}, err
	}
	switch in.Status {
	case ReportInReview, ReportActioned, ReportDismissed:
	default:
		return ReportView{}, problem.Field("body.status", "must be in_review, actioned or dismissed")
	}
	note, err := cleanText("body.resolution_note", in.Note, maxReportText)
	if err != nil {
		return ReportView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		Report uuid.UUID
		In     ResolveInput
	}{"resolve", id, in})
	if err != nil {
		return ReportView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.reports().LockReport(ctx, id)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := authz.Can(ctx, a, authz.FeedReportResolve, reportResource(cur)); err != nil {
			return uuid.Nil(), err
		}
		next, changed, err := s.resolved(ctx, a, cur, in, note)
		if err != nil || !changed {
			return cur.ID, err
		}
		if next, err = s.reports().UpdateReport(ctx, next); err != nil {
			return uuid.Nil(), err
		}
		rid := next.ID
		if err := s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.FeedReportResolve), Outcome: audit.Success, ResourceType: reportAggType, ResourceID: &rid,
			Metadata: map[string]any{"status": string(next.Status), "previous_status": string(cur.Status),
				"target_type": string(next.TargetType), "target_id": next.TargetID.String()},
		}.By(a)); err != nil {
			return uuid.Nil(), err
		}
		if !next.Status.Unresolved() {
			s.d.Queue.AfterCommit(ctx, NotifyReportResolvedTaskName,
				reportResolvedTask{ReportID: next.ID, ReporterUserID: next.ReporterUserID},
				"feed.report_resolved:"+next.ID.String())
		}
		return next.ID, nil
	})
	if err != nil {
		return ReportView{}, err
	}
	r, err := s.reports().GetReport(ctx, id)
	if err != nil {
		return ReportView{}, err
	}
	return s.reportView(ctx, r)
}

// resolved is the report's next state; changed=false when nothing moves.
func (s *service) resolved(ctx context.Context, a authz.Actor, cur Report, in ResolveInput, note *string) (Report, bool, error) {
	next := cur
	me := a.UserID
	if !cur.Status.Unresolved() {
		if in.Status == cur.Status && note == nil && !in.AssignToMe {
			return cur, false, nil // the same outcome again
		}
		return Report{}, false, ErrInvalidState
	}
	if note != nil {
		next.ResolutionNote = note
	}
	if in.AssignToMe || next.AssignedTo == nil {
		next.AssignedTo = &me
	}
	switch in.Status {
	case ReportInReview:
		next.Status = ReportInReview
	case ReportActioned:
		if err := s.actionedProof(ctx, cur, next.ResolutionNote); err != nil {
			return Report{}, false, err
		}
		now := s.now()
		next.Status, next.ResolvedAt = ReportActioned, &now
	case ReportDismissed:
		now := s.now()
		next.Status, next.ResolvedAt = ReportDismissed, &now
	}
	changed := next.Status != cur.Status || !samePtr(next.AssignedTo, cur.AssignedTo) ||
		!sameStr(next.ResolutionNote, cur.ResolutionNote)
	return next, changed, nil
}

// actionedProof is what an actioned report needs: a moderation action
// taken with it (409 feed.moderation_action_required), or for a message
// report, whose measure is another module's (the company's or the user's
// suspension, the conversation closed; BE-162b), a resolution note saying
// what was done (422).
func (s *service) actionedProof(ctx context.Context, cur Report, note *string) error {
	if cur.TargetType == TargetMessage {
		if note == nil {
			return problem.Field("body.resolution_note", "is required to action a message report: say what was done")
		}
		return nil
	}
	n, err := s.moderation().ReportActions(ctx, cur.ID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrModerationActionRequired
	}
	return nil
}

func samePtr(a, b *uuid.UUID) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func sameStr(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
