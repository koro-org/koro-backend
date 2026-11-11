package ingest

import (
	"slices"
	"time"
	"uuid"
)

// Kind is ingest_sources.kind: what a source yields.
type Kind string

// Source kinds.
const (
	KindClubList Kind = "club_list" // a university's official club list
	KindEvents   Kind = "events"    // an events page, feed, sitemap or JSON
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool { return k == KindClubList || k == KindEvents }

// AdapterName is ingest_sources.adapter: the SourceAdapter that reads the
// fetched body (adapters.go).
type AdapterName string

// Adapter names (ingest_sources_adapter_chk).
const (
	AdapterHTMLPage       AdapterName = "htmlpage"
	AdapterJSONAPI        AdapterName = "jsonapi"
	AdapterRSS            AdapterName = "rss"
	AdapterSitemap        AdapterName = "sitemap"
	AdapterInstagramGraph AdapterName = "instagram_graph" // Aşama 3: the club's own account, never fetched here
	AdapterManualPaste    AdapterName = "manual_paste"    // Aşama 2: "hızlı içe aktar", never fetched here
	AdapterClubListYTU    AdapterName = "clublist_ytu"
	AdapterClubListBOUN   AdapterName = "clublist_boun"
	AdapterClubListSU     AdapterName = "clublist_sabanci"
	AdapterClubListMETU   AdapterName = "clublist_metu"
	AdapterClubListITU    AdapterName = "clublist_itu"
)

// AdapterNames lists every adapter name of the CHECK constraint.
func AdapterNames() []AdapterName {
	return []AdapterName{AdapterHTMLPage, AdapterJSONAPI, AdapterRSS, AdapterSitemap, AdapterInstagramGraph,
		AdapterManualPaste, AdapterClubListYTU, AdapterClubListBOUN, AdapterClubListSU, AdapterClubListMETU,
		AdapterClubListITU}
}

// Valid reports whether a is a known adapter name.
func (a AdapterName) Valid() bool { return slices.Contains(AdapterNames(), a) }

// PausedReason is ingest_sources.paused_reason: why a source is disabled.
type PausedReason string

// Paused reasons.
const (
	PausedAdmin    PausedReason = "admin"    // a platform admin turned it off
	PausedFailures PausedReason = "failures" // fail_count reached the limit
	PausedClaimed  PausedReason = "claimed"  // the club claimed its profile
	PausedRobots   PausedReason = "robots"   // robots.txt refuses the URL
)

// Outcome is ingest_runs.outcome.
type Outcome string

// Run outcomes.
const (
	OutcomeOK           Outcome = "ok"            // fetched, parsed, items written
	OutcomeUnchanged    Outcome = "unchanged"     // same body hash: nothing parsed or extracted
	OutcomeNotModified  Outcome = "not_modified"  // 304 to the conditional GET
	OutcomeRobotsDenied Outcome = "robots_denied" // robots.txt refuses the URL: not fetched
	OutcomeBlocked      Outcome = "blocked"       // SSRF guard: the address is not public
	OutcomeHTTPError    Outcome = "http_error"    // non-2xx answer
	OutcomeFetchError   Outcome = "fetch_error"   // network, size, time or content type
	OutcomeParseError   Outcome = "parse_error"   // the adapter could not read the body
	OutcomeExtractError Outcome = "extract_error" // the extractor failed
	OutcomeUnsupported  Outcome = "unsupported"   // an adapter that never fetches (instagram_graph, manual_paste)
)

// Failed reports whether the outcome counts towards fail_count.
func (o Outcome) Failed() bool {
	switch o {
	case OutcomeOK, OutcomeUnchanged, OutcomeNotModified:
		return false
	}
	return true
}

// Status is imported_events.status.
type Status string

// Imported event statuses.
const (
	StatusImported Status = "imported" // shown on the public surfaces
	StatusHidden   Status = "hidden"   // hidden by a platform admin or a removal request
	StatusPromoted Status = "promoted" // the club's admin made it a draft event
	StatusRejected Status = "rejected" // the club's admin declined it
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	return s == StatusImported || s == StatusHidden || s == StatusPromoted || s == StatusRejected
}

// HiddenReason is imported_events.hidden_reason.
type HiddenReason string

// Hidden reasons.
const (
	HiddenByAdmin   HiddenReason = "admin"
	HiddenByRequest HiddenReason = "removal_request"
)

// Category is imported_events.category: the events.category values (an
// imported event never carries a sensitive flag; "other" when unknown).
type Category string

// Categories lists the accepted categories (imported_events_category_chk).
func Categories() []Category {
	return []Category{"workshop", "talk", "competition", "career", "social", "trip", "sports", "culture",
		"volunteering", "other"}
}

// Valid reports whether c is a known category.
func (c Category) Valid() bool { return slices.Contains(Categories(), c) }

// CategoryOther is the default category.
const CategoryOther Category = "other"

// Source is one ingest_sources row.
type Source struct {
	ID              uuid.UUID     `gorm:"type:uuid;column:id;primaryKey"`
	Kind            Kind          `gorm:"column:kind"`
	Adapter         AdapterName   `gorm:"column:adapter"`
	URL             string        `gorm:"column:url"`
	Host            string        `gorm:"column:host"`
	Label           string        `gorm:"column:label"`
	UniversityID    uuid.UUID     `gorm:"type:uuid;column:university_id"`
	ClubOrgID       *uuid.UUID    `gorm:"type:uuid;column:club_org_id"`
	ParentID        *uuid.UUID    `gorm:"type:uuid;column:parent_id"`
	IntervalSeconds int           `gorm:"column:interval_seconds"`
	Enabled         bool          `gorm:"column:enabled"`
	PausedReason    *PausedReason `gorm:"column:paused_reason"`
	NextRunAt       *time.Time    `gorm:"column:next_run_at"`
	ETag            *string       `gorm:"column:etag"`
	LastModified    *string       `gorm:"column:last_modified"`
	ContentHash     []byte        `gorm:"column:content_hash"`
	FailCount       int           `gorm:"column:fail_count"`
	ZeroYieldRuns   int           `gorm:"column:zero_yield_runs"`
	LastError       *string       `gorm:"column:last_error"`
	LastStatus      *int          `gorm:"column:last_status"`
	LastYield       *int          `gorm:"column:last_yield"`
	LastFetchedAt   *time.Time    `gorm:"column:last_fetched_at"`
	LastOKAt        *time.Time    `gorm:"column:last_ok_at"`
	CreatedAt       time.Time     `gorm:"column:created_at;->"`
	UpdatedAt       time.Time     `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Source) TableName() string { return "ingest_sources" }

// Interval returns the fetch period (0: fetched once).
func (s Source) Interval() time.Duration { return time.Duration(s.IntervalSeconds) * time.Second }

// Run is one ingest_runs row.
type Run struct {
	ID           uuid.UUID `gorm:"type:uuid;column:id;primaryKey"`
	SourceID     uuid.UUID `gorm:"type:uuid;column:source_id"`
	StartedAt    time.Time `gorm:"column:started_at"`
	FinishedAt   time.Time `gorm:"column:finished_at"`
	Outcome      Outcome   `gorm:"column:outcome"`
	HTTPStatus   *int      `gorm:"column:http_status"`
	Bytes        int       `gorm:"column:bytes"`
	Found        int       `gorm:"column:found"`
	Created      int       `gorm:"column:created"`
	Updated      int       `gorm:"column:updated"`
	Rejected     int       `gorm:"column:rejected"`
	InputTokens  int       `gorm:"column:input_tokens"`
	OutputTokens int       `gorm:"column:output_tokens"`
	ErrorCode    *string   `gorm:"column:error_code"`
}

// TableName implements gorm's Tabler.
func (Run) TableName() string { return "ingest_runs" }

// ImportedClub is one imported_clubs row: a club seen on an official list.
type ImportedClub struct {
	ClubOrgID      uuid.UUID  `gorm:"type:uuid;column:club_org_id;primaryKey"`
	UniversityID   uuid.UUID  `gorm:"type:uuid;column:university_id"`
	SourceID       *uuid.UUID `gorm:"type:uuid;column:source_id"`
	NameKey        string     `gorm:"column:name_key"`
	SourceCategory *string    `gorm:"column:source_category"`
	NeedsReview    bool       `gorm:"column:needs_review"`
	ContactEmail   *string    `gorm:"column:contact_email"`
	FirstSeenAt    time.Time  `gorm:"column:first_seen_at"`
	LastSeenAt     time.Time  `gorm:"column:last_seen_at"`
	MissingSince   *time.Time `gorm:"column:missing_since"`
	CreatedAt      time.Time  `gorm:"column:created_at;->"`
	UpdatedAt      time.Time  `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (ImportedClub) TableName() string { return "imported_clubs" }

// ImportedEvent is one imported_events row.
type ImportedEvent struct {
	ID              uuid.UUID     `gorm:"type:uuid;column:id;primaryKey"`
	SourceID        *uuid.UUID    `gorm:"type:uuid;column:source_id"`
	UniversityID    uuid.UUID     `gorm:"type:uuid;column:university_id"`
	ClubOrgID       *uuid.UUID    `gorm:"type:uuid;column:club_org_id"`
	Status          Status        `gorm:"column:status"`
	HiddenReason    *HiddenReason `gorm:"column:hidden_reason"`
	Title           string        `gorm:"column:title"`
	StartsAt        time.Time     `gorm:"column:starts_at"`
	EndsAt          *time.Time    `gorm:"column:ends_at"`
	AllDay          bool          `gorm:"column:all_day"`
	LocationText    *string       `gorm:"column:location_text"`
	Category        Category      `gorm:"column:category"`
	SourceURL       string        `gorm:"column:source_url"`
	Evidence        string        `gorm:"column:evidence"`
	Confidence      float64       `gorm:"column:confidence"`
	Fingerprint     []byte        `gorm:"column:fingerprint"`
	PromotedEventID *uuid.UUID    `gorm:"type:uuid;column:promoted_event_id"`
	ExpiresAt       time.Time     `gorm:"column:expires_at"`
	LastSeenAt      time.Time     `gorm:"column:last_seen_at"`
	CreatedAt       time.Time     `gorm:"column:created_at;->"`
	UpdatedAt       time.Time     `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (ImportedEvent) TableName() string { return "imported_events" }

// End returns the event's end, its start when it has none.
func (e ImportedEvent) End() time.Time {
	if e.EndsAt != nil {
		return *e.EndsAt
	}
	return e.StartsAt
}
