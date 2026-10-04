// Package outreach is the targeting module (01 §4.13; BE-030): segments
// (saved audiences of an org), the audience count (Seviye 0) and
// Targeting.Compile, which fixes a targeting for a post. The notification
// dispatch of a targeted post (ScheduleDispatch, outreach_dispatches, the
// worker) comes with BE-035.
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
	"koro/internal/platform/idempotency"
	"koro/internal/platform/queue"
)

// Targeting is the contract of the feed (01 §4.13; BE-032 targeted posts).
// ScheduleDispatch joins it with BE-035.
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
}

// Job name (01 §7; <module>.<verb>_<object>).
const (
	// RecountSegmentsJobName refreshes the estimate of every active segment
	// (k < 10 → NULL) at 06:00 Europe/Istanbul on the low queue.
	RecountSegmentsJobName = "outreach.recount_segments"
)

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
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("outreach.Deps.%s is missing", c.name))
		}
	}
	if d.DailyCountLimit < 0 {
		errs = append(errs, errors.New("outreach.Deps.DailyCountLimit is negative"))
	}
	return errors.Join(errs...)
}

// Module is what outreach hands to the composition root.
type Module struct {
	Service Service

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
	s := &service{d: d}
	return Module{Service: s, svc: s}, nil
}

// Jobs returns the scheduled jobs of outreach (01 §7).
func (m Module) Jobs() []queue.Job {
	return []queue.Job{
		{Name: RecountSegmentsJobName, Cron: "0 6 * * *", Queue: queue.Low, Unique: 23 * time.Hour, Handle: m.svc.recountSegments},
	}
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
