package ingest

import (
	"time"
	"uuid"
)

// The JSON shapes of the ingest operations (04 §2.25 doc patch).

// SourceLabel is the label every imported event carries on the screens
// ("Otomatik toplandı · <source_host>").
const SourceLabel = "Otomatik toplandı"

// ImportedEventClubDTO names the club of an imported event.
type ImportedEventClubDTO struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Slug string    `json:"slug"`
}

// ImportedEventDTO is an imported event on the public surfaces: a card
// with its source, never a registration, a cover or a person.
type ImportedEventDTO struct {
	ID           uuid.UUID             `json:"id"`
	Kind         string                `json:"kind" enum:"imported" doc:"Always imported: not a Koro event (no registration, no talent, no recommendation)"`
	Label        string                `json:"label" doc:"Otomatik toplandı: show it with source_host and link source_url"`
	Title        string                `json:"title"`
	StartsAt     time.Time             `json:"starts_at"`
	EndsAt       *time.Time            `json:"ends_at,omitempty"`
	AllDay       bool                  `json:"all_day" doc:"The source gives a date without a time"`
	LocationText string                `json:"location_text,omitempty" doc:"The place as the source writes it"`
	Category     string                `json:"category" enum:"workshop,talk,competition,career,social,trip,sports,culture,volunteering,other"`
	SourceURL    string                `json:"source_url"`
	SourceHost   string                `json:"source_host"`
	UniversityID uuid.UUID             `json:"university_id"`
	Club         *ImportedEventClubDTO `json:"club,omitempty"`
}

// ImportedEventSuggestionDTO is an imported event as its club's admin sees it: with the
// quote the extractor cited.
type ImportedEventSuggestionDTO struct {
	ImportedEventDTO
	Evidence string `json:"evidence" doc:"The quote of the source page the event was read from"`
}

// AdminImportedEventDTO is an imported event on the admin surface.
type AdminImportedEventDTO struct {
	ImportedEventDTO
	Status          string     `json:"status" enum:"imported,hidden,promoted,rejected"`
	HiddenReason    string     `json:"hidden_reason,omitempty" enum:"admin,removal_request"`
	Evidence        string     `json:"evidence"`
	Confidence      float64    `json:"confidence"`
	SourceID        *uuid.UUID `json:"source_id,omitempty"`
	PromotedEventID *uuid.UUID `json:"promoted_event_id,omitempty"`
	ExpiresAt       time.Time  `json:"expires_at"`
	LastSeenAt      time.Time  `json:"last_seen_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

// ImportedEventList is a page of public imported events.
type ImportedEventList struct {
	Items      []ImportedEventDTO `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// ImportedEventSuggestionList is a page of a club's suggestions.
type ImportedEventSuggestionList struct {
	Items      []ImportedEventSuggestionDTO `json:"items"`
	NextCursor string                       `json:"next_cursor,omitempty"`
}

// AdminImportedEventList is a page of the admin's list.
type AdminImportedEventList struct {
	Items      []AdminImportedEventDTO `json:"items"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

// ImportedEventPromotedDTO is the answer of promoteImportedEvent.
type ImportedEventPromotedDTO struct {
	ImportedEventID uuid.UUID `json:"imported_event_id"`
	EventID         uuid.UUID `json:"event_id" doc:"The new draft event (getEvent, updateEvent, publishEvent)"`
}

// IngestRunDTO is one fetch of a source.
type IngestRunDTO struct {
	ID           uuid.UUID `json:"id"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	Outcome      string    `json:"outcome" enum:"ok,unchanged,not_modified,robots_denied,blocked,http_error,fetch_error,parse_error,extract_error,unsupported"`
	HTTPStatus   *int      `json:"http_status,omitempty"`
	Bytes        int       `json:"bytes"`
	Found        int       `json:"found"`
	Created      int       `json:"created"`
	Updated      int       `json:"updated"`
	Rejected     int       `json:"rejected"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	ErrorCode    string    `json:"error_code,omitempty"`
}

// IngestSourceDTO is a source on the admin surface (no body, no hash).
type IngestSourceDTO struct {
	ID            uuid.UUID  `json:"id"`
	Kind          string     `json:"kind" enum:"club_list,events"`
	Adapter       string     `json:"adapter"`
	URL           string     `json:"url"`
	Host          string     `json:"host"`
	Label         string     `json:"label"`
	UniversityID  uuid.UUID  `json:"university_id"`
	ClubID        *uuid.UUID `json:"club_id,omitempty"`
	ParentID      *uuid.UUID `json:"parent_id,omitempty" doc:"The sitemap that named this page (fetched once)"`
	IntervalHours int        `json:"interval_hours" doc:"0: fetched once"`
	Enabled       bool       `json:"enabled"`
	PausedReason  string     `json:"paused_reason,omitempty" enum:"admin,failures,claimed,robots"`
	NextRunAt     *time.Time `json:"next_run_at,omitempty"`
	FailCount     int        `json:"fail_count"`
	ZeroYieldRuns int        `json:"zero_yield_runs" doc:"Successful runs in a row that found nothing"`
	LastError     string     `json:"last_error,omitempty"`
	LastStatus    *int       `json:"last_status,omitempty"`
	LastYield     *int       `json:"last_yield,omitempty"`
	LastFetchedAt *time.Time `json:"last_fetched_at,omitempty"`
	LastOKAt      *time.Time `json:"last_ok_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// IngestSourceDetailDTO is a source with its latest runs (newest first).
type IngestSourceDetailDTO struct {
	IngestSourceDTO
	Runs []IngestRunDTO `json:"runs"`
}

// IngestSourceList is a page of sources.
type IngestSourceList struct {
	Items      []IngestSourceDTO `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// IngestPilotResultDTO is the answer of adminAddPilotIngestSources.
type IngestPilotResultDTO struct {
	Created  int               `json:"created"`
	Existing int               `json:"existing"`
	Sources  []IngestSourceDTO `json:"sources"`
}

// ImportedClubDTO is a club seen on an official list.
type ImportedClubDTO struct {
	ClubID         uuid.UUID  `json:"club_id"`
	Name           string     `json:"name"`
	Slug           string     `json:"slug"`
	UniversityID   uuid.UUID  `json:"university_id"`
	SourceID       *uuid.UUID `json:"source_id,omitempty"`
	SourceCategory string     `json:"source_category,omitempty" doc:"The university's own category label"`
	NeedsReview    bool       `json:"needs_review" doc:"Started as other: a special category or an unknown label (adminSetClubSensitivity, updateClub)"`
	ContactEmail   string     `json:"contact_email,omitempty" doc:"The club's group address on the university's domain"`
	FirstSeenAt    time.Time  `json:"first_seen_at"`
	LastSeenAt     time.Time  `json:"last_seen_at"`
	MissingSince   *time.Time `json:"missing_since,omitempty" doc:"The list stopped naming the club"`
}

// ImportedClubList is a page of imported clubs.
type ImportedClubList struct {
	Items      []ImportedClubDTO `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

func toEventDTO(v EventView) ImportedEventDTO {
	e := v.Event
	d := ImportedEventDTO{
		ID: e.ID, Kind: "imported", Label: SourceLabel, Title: e.Title, StartsAt: e.StartsAt, EndsAt: e.EndsAt,
		AllDay: e.AllDay, LocationText: deref(e.LocationText), Category: string(e.Category), SourceURL: e.SourceURL,
		SourceHost: v.SourceHost, UniversityID: e.UniversityID,
	}
	if v.Club != nil {
		d.Club = &ImportedEventClubDTO{ID: v.Club.ID, Name: v.Club.Name, Slug: v.Club.Slug}
	}
	return d
}

func toSuggestionDTO(v EventView) ImportedEventSuggestionDTO {
	return ImportedEventSuggestionDTO{ImportedEventDTO: toEventDTO(v), Evidence: v.Event.Evidence}
}

func toAdminEventDTO(v EventView) AdminImportedEventDTO {
	e := v.Event
	d := AdminImportedEventDTO{
		ImportedEventDTO: toEventDTO(v), Status: string(e.Status), Evidence: e.Evidence, Confidence: e.Confidence,
		SourceID: e.SourceID, PromotedEventID: e.PromotedEventID, ExpiresAt: e.ExpiresAt, LastSeenAt: e.LastSeenAt,
		CreatedAt: e.CreatedAt,
	}
	if e.HiddenReason != nil {
		d.HiddenReason = string(*e.HiddenReason)
	}
	return d
}

func toSourceDTO(s Source) IngestSourceDTO {
	d := IngestSourceDTO{
		ID: s.ID, Kind: string(s.Kind), Adapter: string(s.Adapter), URL: s.URL, Host: s.Host, Label: s.Label,
		UniversityID: s.UniversityID, ClubID: s.ClubOrgID, ParentID: s.ParentID, IntervalHours: s.IntervalSeconds / 3600,
		Enabled: s.Enabled, NextRunAt: s.NextRunAt, FailCount: s.FailCount, ZeroYieldRuns: s.ZeroYieldRuns,
		LastError: deref(s.LastError), LastStatus: s.LastStatus, LastYield: s.LastYield, LastFetchedAt: s.LastFetchedAt,
		LastOKAt: s.LastOKAt, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
	if s.PausedReason != nil {
		d.PausedReason = string(*s.PausedReason)
	}
	return d
}

func toRunDTO(r Run) IngestRunDTO {
	return IngestRunDTO{
		ID: r.ID, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Outcome: string(r.Outcome), HTTPStatus: r.HTTPStatus,
		Bytes: r.Bytes, Found: r.Found, Created: r.Created, Updated: r.Updated, Rejected: r.Rejected,
		InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, ErrorCode: deref(r.ErrorCode),
	}
}

func toSourceDetail(v SourceView) IngestSourceDetailDTO {
	d := IngestSourceDetailDTO{IngestSourceDTO: toSourceDTO(v.Source), Runs: make([]IngestRunDTO, len(v.Runs))}
	for i, r := range v.Runs {
		d.Runs[i] = toRunDTO(r)
	}
	return d
}

func toClubDTO(c ClubItem) ImportedClubDTO {
	return ImportedClubDTO{
		ClubID: c.Club.ClubOrgID, Name: c.Name, Slug: c.Slug, UniversityID: c.Club.UniversityID, SourceID: c.Club.SourceID,
		SourceCategory: deref(c.Club.SourceCategory), NeedsReview: c.Club.NeedsReview,
		ContactEmail: deref(c.Club.ContactEmail), FirstSeenAt: c.Club.FirstSeenAt, LastSeenAt: c.Club.LastSeenAt,
		MissingSince: c.Club.MissingSince,
	}
}
