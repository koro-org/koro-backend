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

// TargetSummary is the reported post or org as a moderator sees it.
type TargetSummary struct {
	Type        TargetType
	Org         orgs.OrgSummary
	Post        *Post      // a post target
	TrustLevel  TrustLevel // the author org's (or the org's) feed trust level
	OpenReports int        // distinct users with an open report on the target
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
// enough distinct users.
func (s *service) CreateReport(ctx context.Context, a authz.Actor, in ReportInput) (ReportResult, error) {
	if !a.IsUser() {
		return ReportResult{}, authz.ErrForbidden
	}
	if !in.Reason.Valid() {
		return ReportResult{}, problem.Field("body.reason", "is not a report reason")
	}
	details, err := cleanText("body.details", in.Details, maxReportText)
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
	res, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (ReportResult, error) {
		r, created, err := s.reports().InsertReport(ctx, Report{
			ID: uuid.NewV7(), ReporterUserID: a.UserID, TargetType: in.TargetType, TargetID: in.TargetID,
			Reason: in.Reason, Details: details,
		})
		if err != nil || !created {
			return ReportResult{ID: r.ID}, err
		}
		id := r.ID
		if err := s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.FeedReportCreate), Outcome: audit.Success, ResourceType: reportAggType, ResourceID: &id,
			Metadata: map[string]any{"target_type": string(r.TargetType), "target_id": r.TargetID.String(),
				"reason": string(r.Reason)},
		}.By(a)); err != nil {
			return ReportResult{}, err
		}
		s.emitFor(ctx, domainevent.ContentReportCreated{ReportID: r.ID, TargetType: string(r.TargetType)}, reportAggType, r.ID)
		if post != nil {
			if err := s.autoHide(ctx, post.ID); err != nil {
				return ReportResult{}, err
			}
		}
		return ReportResult{ID: r.ID, Created: true}, nil
	})
	return res, err
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

// reportView adds the summary of the report's target.
func (s *service) reportView(ctx context.Context, r Report) (ReportView, error) {
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
	return ReportView{Report: r, Target: t}, nil
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
		n, err := s.moderation().ReportActions(ctx, cur.ID)
		if err != nil {
			return Report{}, false, err
		}
		if n == 0 {
			return Report{}, false, ErrModerationActionRequired
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

func samePtr(a, b *uuid.UUID) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func sameStr(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
