// Package outreach is the targeting module (01 §4.13; BE-030): segments
// (saved audiences of an org), the audience count (Seviye 0) and
// Targeting.Compile, which fixes a targeting for a post; and the
// notification dispatch of a targeted post (BE-035): ScheduleDispatch,
// outreach_dispatches and the worker that notifies the audience.
//
// Rules the package enforces:
//
//   - Targeting (02 §9.1, §13.1; DECISIONS-LOG 2026-10-02): OR inside a
//     dimension, AND across dimensions. The dimensions are the university,
//     the department, the class year and the club; a definition without
//     any is refused. The audience keys and the dimension count are always
//     derived from the definition, never taken from a caller (T-132).
//   - Scope (03 k16): a club_admin targets only club:<own club> (a
//     sensitive club too, without k ≥ 10: the club addresses its own
//     members); a university_rep or career_staff targets uni:<own
//     university>, narrowed by department or year, never a club; a
//     company (and platform_admin) targets every dimension but never a
//     sensitive club (03 Y2: 422 outreach.sensitive_target) and needs
//     k ≥ 10. Anything else is 422 outreach.target_out_of_scope.
//   - k-anonymity (04 §V.2): counts come from students.Audience.Count
//     (count_segment), which sees only audience eligible students: verified,
//     adult, active. Below 10 there is no number at all — the count is
//     null with below_threshold, a compiled targeting that needs k fails
//     with 422 outreach.audience_too_small, a segment's last_count is NULL.
//   - Difference attacks (two counts that differ by a small group): every
//     published count is rounded down to its bucket (Bucket: 10 below 100,
//     50 below 1 000, 100 below 10 000, then 1 000); the exact count is
//     never returned, stored or logged. Counting is rate limited per org:
//     30 per minute at the API (04 §1.9 audience_count: countAudience,
//     createSegment, updateSegment) and a daily budget across every fresh
//     count, Compile included (OUTREACH_COUNT_DAILY_LIMIT, default 300;
//     429 rate_limited, fail closed without the limiter). getSegment shows
//     the stored estimate and costs nothing.
//   - Talent values never enter outreach (01 §0-7): no dimension, no
//     field and no import reads them.
//   - Org status (03 §2.3): segment writes and counts for a suspended org
//     are 403 orgs.org_suspended (platform_admin excepted), for a company
//     awaiting verification 403 orgs.org_not_verified, for a closed org
//     409 orgs.invalid_state; reads stay open.
//   - Segments hold no personal data (02 §13.1: toplu); writes are
//     audited, archiving is a soft delete (archived_at).
//
// Dispatch rules (BE-035; 01 §6.3, 02 §13.2, 03 k18, 04 §2.8, 06 H-61):
//
//   - A dispatch notifies the audience of one targeted post (at most one
//     per post). It is scheduled in the caller's transaction — the feed's
//     targeted post with "notify on publish", or createDispatch — together
//     with a company's targeted_dispatch unit and the outbox event
//     outreach.dispatch_scheduled; it runs once the post is published.
//   - Commercial (6563, İYS): a company's dispatch and the dispatch of a
//     sponsored post are commercial and reach only the students whose
//     commercial consent of the channel is in force (consent.FilterActive;
//     notify checks it again). A club's or university's informational
//     notice needs neither consent nor a unit (T-16). Commercial e-mail
//     carries the one-click unsubscribe link in its footer; commercial
//     messages never go in-app (notify).
//   - Minors and sensitive clubs: a dispatch never reaches a student under
//     18 (every batch is checked against identity again) and never targets
//     a sensitive club's key (03 Y2). A club's notice to its own members —
//     a sensitive club's too — walks the club's member list instead of the
//     audience (T-258), without k ≥ 10.
//   - The run is batched and idempotent: one notification per recipient
//     and channel, deduplicated by notify (dedupe key
//     dispatch:<id>:<user>:<channel>), so a retried or resumed run never
//     notifies anyone twice. Notifications carry no personal data: fixed
//     in-app and push texts, the post's title in e-mail.
//   - Counts (06 H-61, T-260): eligible, consented and sent are stored and
//     shown only as PublicEstimate (null below 10, rounded down); the exact
//     numbers of a run are neither stored, returned nor logged.
package outreach

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// Targeting is the contract of the feed (01 §4.13; BE-032 targeted posts,
// BE-035 their dispatch).
type Targeting interface {
	// Compile fixes a targeting of spec.OrgID for actor a: it validates
	// the definition (or loads the saved segment), applies a's scope in
	// the org (03 k16) and sizes the audience. It returns
	// ErrTargetOutOfScope, ErrSensitiveTarget, ErrAudienceTooSmall (no
	// number; not for a club addressing its own members), ErrNotFound (no
	// such segment in the org), ErrInvalidState (archived segment) or a
	// validation error. Each call spends one unit of the org's daily count
	// budget (ErrRateLimited). The caller has authorised its own action
	// (feed.post.create_targeted): Compile does not call authz.Can.
	Compile(ctx context.Context, a authz.Actor, spec SegmentSpec) (Compiled, error)
	// ScheduleDispatch schedules the notification of in.Post's audience in
	// the caller's transaction (01 §6.3): authz (outreach.dispatch.create,
	// 03 k18), the org's status, the scope of in.Actor, the channels and
	// the commercial nature (422), a company's targeted_dispatch unit (403
	// entitlements.not_entitled), the row (409 outreach.dispatch_exists),
	// outreach.dispatch_scheduled (O) and the audit record. The dispatch
	// runs once the post is published. It returns the dispatch ID.
	ScheduleDispatch(ctx context.Context, in DispatchInput) (uuid.UUID, error)
	// DiscardDispatch removes the dispatch of a post that leaves before it
	// was ever published (a deleted draft, a post rejected in review), in
	// the caller's transaction: its unit goes back and its row is deleted
	// (the post row may go). a is who withdrew the post (audited). No
	// dispatch is a no-op; a dispatch that ran is ErrInvalidState.
	DiscardDispatch(ctx context.Context, a authz.Actor, orgID, postID uuid.UUID) error
	// DispatchOfPost returns a post's dispatch, if any, with its counts
	// as published estimates only (getPostStats' dispatch; 06 H-61). The
	// caller has authorised its own read of the post.
	DispatchOfPost(ctx context.Context, orgID, postID uuid.UUID) (DispatchView, bool, error)
	// DispatchIDsOfPosts returns the dispatch ID of each of the org's posts
	// that has one (the management list's dispatch_id, BE-127). The caller
	// has authorised its read of the org's posts.
	DispatchIDsOfPosts(ctx context.Context, orgID uuid.UUID, postIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
}

// Service is the outreach service: the cross-module contract plus the
// HTTP use cases of BE-030 (04 §2.8). Every method that serves an HTTP
// operation calls authz.Can first (05 §5.3).
type Service interface {
	Targeting

	// ListSegments is listSegments (outreach.segment.list).
	ListSegments(ctx context.Context, a authz.Actor, orgID uuid.UUID, q ListQuery) (SegmentPage, error)
	// GetSegment is getSegment (outreach.segment.read): the stored estimate.
	GetSegment(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (SegmentView, error)
	// CreateSegment is createSegment (outreach.segment.manage, k16): the
	// segment with its first estimate.
	CreateSegment(ctx context.Context, a authz.Actor, orgID uuid.UUID, in SegmentInput) (SegmentView, error)
	// UpdateSegment is updateSegment (outreach.segment.manage, k16):
	// ifMatch is the If-Match header (missing → 428, stale → 412); a new
	// definition is counted again.
	UpdateSegment(ctx context.Context, a authz.Actor, orgID, id uuid.UUID, ifMatch string, p SegmentPatch) (SegmentView, error)
	// ArchiveSegment is archiveSegment (outreach.segment.manage): archiving
	// an archived segment changes nothing.
	ArchiveSegment(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (SegmentView, error)
	// CountAudience is countAudience (outreach.audience.count, k16 for a
	// club_admin, U for a university_rep): the estimate of a definition.
	CountAudience(ctx context.Context, a authz.Actor, orgID uuid.UUID, def Definition) (CountResult, error)

	// CreateDispatch is createDispatch (outreach.dispatch.create, k18): the
	// dispatch of a targeted post (the post's org from the post row, 06
	// H-38), scheduled under the request's Idempotency-Key.
	CreateDispatch(ctx context.Context, a authz.Actor, postID uuid.UUID, req DispatchRequest) (DispatchView, error)
	// ListDispatches is listDispatches (outreach.dispatch.list).
	ListDispatches(ctx context.Context, a authz.Actor, orgID uuid.UUID, q DispatchQuery) (DispatchPage, error)
	// GetDispatch is getDispatch (outreach.dispatch.read): the counts as
	// published estimates only.
	GetDispatch(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (DispatchView, error)
	// CancelDispatch is cancelDispatch (outreach.dispatch.cancel): a
	// scheduled dispatch only (409 otherwise); a company's unit goes back.
	CancelDispatch(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (DispatchView, error)
}

// Job and task names (01 §7, §6.2; <module>.<verb>_<object>).
const (
	// RecountSegmentsJobName refreshes the estimate of every active segment
	// (k < 10 → NULL) at 06:00 Europe/Istanbul on the low queue.
	RecountSegmentsJobName = "outreach.recount_segments"
	// RunDispatchTaskName consumes outreach.dispatch_scheduled (O): the
	// dispatch runs if its post is published, waits otherwise.
	RunDispatchTaskName = "outreach.run_dispatch"
	// StartDispatchTaskName consumes post.published (A): the waiting
	// dispatch of the post runs.
	StartDispatchTaskName = "outreach.start_dispatch"
	// ResumeDispatchesJobName runs every 10 minutes: scheduled dispatches
	// whose post is published (a lost post.published) and runs abandoned
	// by a crashed worker (an expired claim) run again.
	ResumeDispatchesJobName = "outreach.resume_dispatches"
)

// DefaultLease is how long a running dispatch's claim holds without a
// heartbeat (every batch renews it) before another worker may resume it.
const DefaultLease = 15 * time.Minute

// dispatchTimeout bounds one attempt of a run (asynq): a run renews its
// claim per batch, an interrupted one is resumed from the start (notify's
// dedupe keys skip what was sent).
const dispatchTimeout = 30 * time.Minute

// DefaultDailyCountLimit is the daily count budget of an org when Deps
// sets none (OUTREACH_COUNT_DAILY_LIMIT).
const DefaultDailyCountLimit = 300

// Deps are the dependencies of the outreach module. Every field except
// DailyCountLimit is required (New returns an error otherwise).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Audit       AuditLogger         // every segment write, same transaction
	Audience    Audience            // students: count_segment (k ≥ 10)
	Catalog     Catalog             // directory: sensitive clubs, a university's org
	Orgs        Orgs                // orgs: the org's existence and status
	Limiter     Limiter             // the daily count budget per org
	Idempotency *idempotency.Keeper // optional Idempotency-Key of createSegment and archiveSegment (06 H-5)
	Clock       clock.Clock
	Log         *slog.Logger

	// DailyCountLimit is the number of fresh counts an org may run in 24
	// hours (countAudience, createSegment, updateSegment with a definition,
	// Compile). 0 → DefaultDailyCountLimit.
	DailyCountLimit int

	// The dispatch (BE-035).
	Outbox       outbox.Writer // outreach.dispatch_scheduled (O), in the scheduling transaction
	Entitlements Entitlements  // a company's targeted_dispatch unit
	Recipients   Recipients    // students: the audience walk (Stream)
	Members      Members       // orgs: a club's members (its notice to its own members, T-258)
	Users        Users         // identity: active adult accounts, language, e-mail address
	Consents     Consents      // consent: the commercial filter
	Sender       Sender        // notify
	Posts        Posts         // feed (bound late by cmd/koro)

	// Unsubscribe mints each recipient's one-click unsubscribe link of
	// commercial e-mail (consent, BE-062; notify puts it in the footer and
	// the List-Unsubscribe headers). nil → a commercial dispatch cannot use
	// e-mail (CONSENT_UNSUBSCRIBE_BASE_URL unset).
	Unsubscribe UnsubscribeLinks
	// LinkBaseURL is the web origin of the post link in e-mail
	// (IDENTITY_LINK_BASE_URL). "" → no dispatch uses e-mail.
	LinkBaseURL string
	// Lease is how long a run's claim holds without a heartbeat; 0 →
	// DefaultLease.
	Lease time.Duration
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Audit", d.Audit != nil}, {"Audience", d.Audience != nil},
		{"Catalog", d.Catalog != nil}, {"Orgs", d.Orgs != nil}, {"Limiter", d.Limiter != nil},
		{"Idempotency", d.Idempotency != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
		{"Outbox", d.Outbox != nil}, {"Entitlements", d.Entitlements != nil}, {"Recipients", d.Recipients != nil},
		{"Members", d.Members != nil}, {"Users", d.Users != nil}, {"Consents", d.Consents != nil},
		{"Sender", d.Sender != nil}, {"Posts", d.Posts != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("outreach.Deps.%s is missing", c.name))
		}
	}
	if d.DailyCountLimit < 0 {
		errs = append(errs, errors.New("outreach.Deps.DailyCountLimit is negative"))
	}
	if d.Lease < 0 {
		errs = append(errs, errors.New("outreach.Deps.Lease is negative"))
	}
	return errors.Join(errs...)
}

// Module is what outreach hands to the composition root.
type Module struct {
	Service Service
	// Sweeper is the retention sweeper of outreach_dispatches
	// (outreach_dispatch, 2 years; BE-057 pattern).
	Sweeper privacy.RetentionSweeper

	svc *service
}

// New builds the outreach module; a missing dependency is a wiring bug
// that stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	if d.DailyCountLimit == 0 {
		d.DailyCountLimit = DefaultDailyCountLimit
	}
	if d.Lease == 0 {
		d.Lease = DefaultLease
	}
	s := &service{d: d}
	return Module{Service: s, Sweeper: sweeper{s: s}, svc: s}, nil
}

// Jobs returns the scheduled jobs of outreach (01 §7).
func (m Module) Jobs() []queue.Job {
	return []queue.Job{
		{Name: RecountSegmentsJobName, Cron: "0 6 * * *", Queue: queue.Low, Unique: 23 * time.Hour, Handle: m.svc.recountSegments},
		{Name: ResumeDispatchesJobName, Cron: "*/10 * * * *", Queue: queue.Default, Unique: 9 * time.Minute,
			Timeout: dispatchTimeout, Handle: m.svc.resumeDispatches},
	}
}

// Subscribe registers the outbox subscriptions of outreach (01 §6.2):
// outreach.dispatch_scheduled → outreach.run_dispatch and post.published →
// outreach.start_dispatch, on the default queue with a run's timeout.
func (m Module) Subscribe(r *outbox.Registry) {
	q, to := outbox.Queue(queue.Default), outbox.Timeout(dispatchTimeout)
	r.Subscribe(domainevent.TopicOutreachDispatchScheduled, RunDispatchTaskName, m.svc.handleDispatchScheduled, q, to)
	r.Subscribe(domainevent.TopicPostPublished, StartDispatchTaskName, m.svc.handlePostPublished, q, to)
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
