package outreach

import (
	"slices"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// Segment is a segments row (02 §13.1): a saved targeting of an org.
// last_count is the published estimate (Estimate: k ≥ 10, bucketed) or
// NULL; the exact number is never stored.
type Segment struct {
	ID               uuid.UUID    `gorm:"type:uuid;column:id;primaryKey"`
	OrgID            uuid.UUID    `gorm:"type:uuid;column:org_id"`
	Name             string       `gorm:"column:name"`
	Definition       Definition   `gorm:"column:definition;type:jsonb"`
	AudienceKeys     db.TextArray `gorm:"column:audience_keys;type:text[]"`
	AudienceDimCount int16        `gorm:"column:audience_dim_count"`
	LastCount        *int         `gorm:"column:last_count"`
	LastCountedAt    *time.Time   `gorm:"column:last_counted_at"`
	CreatedBy        uuid.UUID    `gorm:"type:uuid;column:created_by"`
	ArchivedAt       *time.Time   `gorm:"column:archived_at"`
	CreatedAt        time.Time    `gorm:"column:created_at;->"`
	UpdatedAt        time.Time    `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Segment) TableName() string { return "segments" }

// Archived reports whether the segment is archived (soft delete).
func (s Segment) Archived() bool { return s.ArchivedAt != nil }

func (s Segment) clone() Segment {
	c := s
	c.Definition = s.Definition.clone()
	c.AudienceKeys = slices.Clone(s.AudienceKeys)
	if s.LastCount != nil {
		n := *s.LastCount
		c.LastCount = &n
	}
	c.LastCountedAt, c.ArchivedAt = cloneTime(s.LastCountedAt), cloneTime(s.ArchivedAt)
	return c
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// Estimate is the only audience number outreach ever publishes (04 §V.2,
// Seviye 0): below k = 10 there is no number at all (BelowThreshold); from
// 10 on, the count is rounded down to its bucket (Bucket), so two counts
// that differ by a few students usually show the same number. The exact
// count stays inside outreach: it is not returned, stored or logged.
type Estimate struct {
	Count          *int // nil when BelowThreshold
	BelowThreshold bool
}

// KThreshold is the smallest audience a number is shown for (k-anonymity).
const KThreshold = 10

// PublicEstimate turns an exact audience size into its published form: no
// number below KThreshold, the bucket from KThreshold on. BE-035's dispatch
// counters (eligible, consented, sent) go through it as well: a delivered
// count can fall below 10 after the minors and suspended accounts are
// filtered again, and a raw delivery count is never shown.
func PublicEstimate(n int) Estimate {
	if n < KThreshold {
		return Estimate{BelowThreshold: true}
	}
	b := Bucket(n)
	return Estimate{Count: &b}
}

// Bucket rounds an audience size of at least KThreshold down to its
// bucket: to a multiple of 10 below 100, of 50 below 1 000, of 100 below
// 10 000 and of 1 000 above. A difference of a few students between two
// definitions (the size of a small group) mostly vanishes in the rounding,
// and the budget of counts per org (Targeting) keeps an attacker from
// probing the bucket edges at scale.
func Bucket(n int) int {
	var step int
	switch {
	case n < KThreshold:
		return 0
	case n < 100:
		step = 10
	case n < 1_000:
		step = 50
	case n < 10_000:
		step = 100
	default:
		step = 1_000
	}
	return n - n%step
}

// SegmentSpec is what a targeting compiles (01 §4.13): the org that
// targets and either a saved segment of that org or an inline definition.
type SegmentSpec struct {
	OrgID      uuid.UUID
	SegmentID  *uuid.UUID  // a saved, active segment of OrgID …
	Definition *Definition // … or an inline definition (exactly one of the two)
}

// Compiled is a targeting fixed for a post (01 §4.13): the audience keys
// and dimension count the feed stores on the post (posts.audience_keys,
// audience_dim_count) and BE-035's dispatch streams, the definition they
// were compiled from (posts.audience_snapshot) and the published size
// estimate (posts.audience_size_estimate).
type Compiled struct {
	Keys       []string
	DimCount   int16
	Definition Definition // normalized
	SegmentID  *uuid.UUID // the saved segment, when the spec named one
	// Size is the published estimate (Bucket; ≥ 10). It is 0 only for a
	// club addressing its own members, which needs no k ≥ 10 (02 §12.2):
	// such an audience may be small, or a sensitive club's, whose
	// membership is never counted.
	Size int
}

// Scope is an actor's targeting scope in an org (03 k16).
type Scope string

// Scopes.
const (
	ScopeNone       Scope = ""
	ScopeClub       Scope = "club"       // club_admin: club:<own club> only; no k ≥ 10, sensitive club allowed
	ScopeUniversity Scope = "university" // university_rep, career_staff: uni:<own> (+ dept, year); no club
	ScopeCompany    Scope = "company"    // recruiter, company_admin/owner: every dimension; no sensitive club; k ≥ 10
	ScopePlatform   Scope = "platform"   // platform_admin: as a company
)

// SegmentView is a segment as the API shows it (04 §2.8 SegmentDTO).
type SegmentView struct {
	Segment
	Estimate Estimate
	ETag     string
}

// SegmentPage is a page of listSegments.
type SegmentPage struct {
	Items []SegmentView
	Next  *uuid.UUID // id of the last item when there is a next page
}

// ListQuery is listSegments' query.
type ListQuery struct {
	Archived bool // true: archived segments only; false: active ones
	Limit    int
	After    *uuid.UUID
}

// SegmentInput is createSegment's body.
type SegmentInput struct {
	Name       string
	Definition Definition
}

// SegmentPatch is updateSegment's body: nil fields stay.
type SegmentPatch struct {
	Name       *string
	Definition *Definition
}

// CountResult is countAudience's answer (04 §2.8).
type CountResult struct {
	Estimate
	Dimensions int16
}

// estimateOf is the stored estimate of a segment.
func estimateOf(s Segment) Estimate {
	if s.LastCount == nil {
		return Estimate{BelowThreshold: true}
	}
	n := *s.LastCount
	return Estimate{Count: &n}
}
