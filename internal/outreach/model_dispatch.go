package outreach

import (
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/db"
)

// DispatchStatus is outreach_dispatches.status (02 §13.2).
type DispatchStatus string

// Dispatch statuses. A dispatch is scheduled until a worker claims it
// (running); it ends completed, cancelled (by the org, or because its post
// left the feed before it ran) or failed (it could not run; the reason is
// last_error, a problem code).
const (
	DispatchScheduled DispatchStatus = "scheduled"
	DispatchRunning   DispatchStatus = "running"
	DispatchCompleted DispatchStatus = "completed"
	DispatchCancelled DispatchStatus = "cancelled"
	DispatchFailed    DispatchStatus = "failed"
)

// Terminal reports whether a dispatch in status s never changes again.
func (s DispatchStatus) Terminal() bool {
	return s == DispatchCompleted || s == DispatchCancelled || s == DispatchFailed
}

// Channel is a notification channel of a dispatch
// (outreach_dispatches_channels_chk).
type Channel string

// Dispatch channels.
const (
	ChannelPush  Channel = "push"
	ChannelEmail Channel = "email"
	ChannelInApp Channel = "in_app"
)

// valid reports whether c is a channel of the CHECK constraint.
func (c Channel) valid() bool { return c == ChannelPush || c == ChannelEmail || c == ChannelInApp }

// AuthorKind is the kind of the org that wrote a post (posts.author_org_kind).
type AuthorKind string

// Author kinds.
const (
	AuthorPlatform   AuthorKind = "platform"
	AuthorUniversity AuthorKind = "university"
	AuthorClub       AuthorKind = "club"
	AuthorCompany    AuthorKind = "company"
)

// PostStatus is the status of a post (posts.status).
type PostStatus string

// Post statuses a dispatch cares about.
const (
	PostDraft         PostStatus = "draft"
	PostPendingReview PostStatus = "pending_review"
	PostPublished     PostStatus = "published"
	PostHidden        PostStatus = "hidden"
	PostRemoved       PostStatus = "removed"
)

// PostInfo is what a dispatch needs to know about its post (feed, L9; 01
// §5.3 "feed ↔ outreach": the feed hands the content summary over, outreach
// never imports the feed). Title is the post's notification title (not
// personal data); Keys and DimCount are the targeting the post was compiled
// with (posts.audience_keys).
type PostInfo struct {
	ID         uuid.UUID
	OrgID      uuid.UUID
	AuthorKind AuthorKind
	Status     PostStatus
	Targeted   bool // audience_kind = segment
	Keys       []string
	DimCount   int16
	SegmentID  *uuid.UUID // the saved segment the targeting came from (posts.audience_snapshot)
	Title      string
	Commercial bool // posts.commercial_flag ("Tanıtım")
}

// DispatchInput is what ScheduleDispatch schedules (01 §4.13): the post
// whose audience is notified, by whom, on which channels. Commercial nil
// takes the post's nature (a company post and a sponsored post are
// commercial, 03 k18, T-16); true asks for the consent filter on an
// informational post; false on a commercial post is refused.
type DispatchInput struct {
	Actor      authz.Actor
	Post       PostInfo
	Channels   []Channel
	Commercial *bool
}

// Dispatch is an outreach_dispatches row (02 §13.2): the notification of a
// targeted post's audience, at most one per post. No row per recipient is
// kept (notification_deliveries holds those). The three counters hold the
// published estimates only (PublicEstimate: NULL below 10, rounded down
// otherwise; 06 H-61, T-260): the exact numbers of a run never leave it.
type Dispatch struct {
	ID                 uuid.UUID      `gorm:"type:uuid;column:id;primaryKey"`
	OrgID              uuid.UUID      `gorm:"type:uuid;column:org_id"`
	PostID             uuid.UUID      `gorm:"type:uuid;column:post_id"`
	SegmentID          *uuid.UUID     `gorm:"type:uuid;column:segment_id"`
	AudienceKeys       db.TextArray   `gorm:"column:audience_keys;type:text[]"`
	AudienceDimCount   int16          `gorm:"column:audience_dim_count"`
	Channels           db.TextArray   `gorm:"column:channels;type:text[]"`
	TitleSnapshot      string         `gorm:"column:title_snapshot"`
	DeepLink           string         `gorm:"column:deep_link"`
	Commercial         bool           `gorm:"column:commercial"`
	Status             DispatchStatus `gorm:"column:status"`
	EligibleCount      *int           `gorm:"column:eligible_count"`
	ConsentedCount     *int           `gorm:"column:consented_count"`
	SentCount          *int           `gorm:"column:sent_count"`
	EntitlementUsageID *uuid.UUID     `gorm:"type:uuid;column:entitlement_usage_id"`
	StartedAt          *time.Time     `gorm:"column:started_at"`
	CompletedAt        *time.Time     `gorm:"column:completed_at"`
	LastError          *string        `gorm:"column:last_error"`
	CreatedBy          uuid.UUID      `gorm:"type:uuid;column:created_by"`
	CreatedAt          time.Time      `gorm:"column:created_at;->"`
	UpdatedAt          time.Time      `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Dispatch) TableName() string { return "outreach_dispatches" }

// ChannelList returns the channels of d.
func (d Dispatch) ChannelList() []Channel {
	out := make([]Channel, len(d.Channels))
	for i, c := range d.Channels {
		out[i] = Channel(c)
	}
	return out
}

// memberList reports whether d addresses a club's own members (02 §12.2,
// 03 k16: the only targeting of a club_admin is club:<own club>). Such a
// dispatch walks the club's member list, not students' audience: a
// sensitive club's members are never in an audience (03 Y2) and the
// club's own members need no k ≥ 10 (T-258).
func (d Dispatch) memberList() bool {
	return len(d.AudienceKeys) == 1 && d.AudienceKeys[0] == keyClub+d.OrgID.String()
}

func (d Dispatch) clone() Dispatch {
	c := d
	c.AudienceKeys, c.Channels = slices.Clone(d.AudienceKeys), slices.Clone(d.Channels)
	c.SegmentID, c.EntitlementUsageID = cloneID(d.SegmentID), cloneID(d.EntitlementUsageID)
	c.EligibleCount, c.ConsentedCount, c.SentCount = cloneInt(d.EligibleCount), cloneInt(d.ConsentedCount), cloneInt(d.SentCount)
	c.StartedAt, c.CompletedAt = cloneTime(d.StartedAt), cloneTime(d.CompletedAt)
	if d.LastError != nil {
		s := *d.LastError
		c.LastError = &s
	}
	return c
}

func cloneID(id *uuid.UUID) *uuid.UUID {
	if id == nil {
		return nil
	}
	v := *id
	return &v
}

func cloneInt(n *int) *int {
	if n == nil {
		return nil
	}
	v := *n
	return &v
}

// DispatchCounts are the published estimates of a finished run
// (PublicEstimate of each exact count: nil below 10).
type DispatchCounts struct {
	Eligible, Consented, Sent *int
}

// publicCounts turns the exact counts of a run into what is stored and
// shown (06 H-61, T-260): the exact numbers stay inside the run.
func publicCounts(eligible, consented, sent int) DispatchCounts {
	return DispatchCounts{
		Eligible: PublicEstimate(eligible).Count, Consented: PublicEstimate(consented).Count,
		Sent: PublicEstimate(sent).Count,
	}
}

// DispatchView is a dispatch as the API shows it (04 §2.8 DispatchDTO).
// The counts go through PublicEstimate again on the way out, so even a row
// written outside the service never shows a number below 10 or off its
// bucket.
type DispatchView struct {
	Dispatch
	Counts DispatchCounts
}

// DispatchPage is a page of listDispatches.
type DispatchPage struct {
	Items []DispatchView
	Next  *uuid.UUID // id of the last item when there is a next page
}

// DispatchQuery is listDispatches' query.
type DispatchQuery struct {
	Limit int
	After *uuid.UUID
}

// DispatchRequest is createDispatch's body (04 §2.8): the channels and
// whether the dispatch is commercial (nil: the post's nature).
type DispatchRequest struct {
	Channels   []Channel
	Commercial *bool
}

// viewOf is the API view of d: every counter through PublicEstimate.
func viewOf(d Dispatch) DispatchView {
	pub := func(n *int) *int {
		if n == nil {
			return nil
		}
		return PublicEstimate(*n).Count
	}
	return DispatchView{Dispatch: d, Counts: DispatchCounts{
		Eligible: pub(d.EligibleCount), Consented: pub(d.ConsentedCount), Sent: pub(d.SentCount),
	}}
}
