package events

import (
	"encoding/json"
	"time"
)

// displayTimezone is the fixed display zone of every event response (04
// §1.12): the API speaks UTC, clients show Europe/Istanbul.
const displayTimezone = "Europe/Istanbul"

// EventOrgDTO is the organiser of an event (public org data only).
type EventOrgDTO struct {
	OrgID string `json:"org_id"`
	Kind  string `json:"kind" enum:"platform,university,club"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
}

// EventCardDTO is an event in a list (04 §2.6 listEvents, listOrgEvents).
// It carries no personal data: no creator, no staff, no online link.
type EventCardDTO struct {
	EventID              string      `json:"event_id"`
	Org                  EventOrgDTO `json:"org"`
	Title                string      `json:"title"`
	Slug                 string      `json:"slug"`
	Summary              string      `json:"summary,omitempty"`
	Category             string      `json:"category"`
	Format               string      `json:"format" enum:"in_person,online,hybrid"`
	LocationName         string      `json:"location_name,omitempty"`
	City                 string      `json:"city,omitempty"`
	StartsAt             time.Time   `json:"starts_at"`
	EndsAt               time.Time   `json:"ends_at"`
	DisplayTimezone      string      `json:"display_timezone" enum:"Europe/Istanbul"`
	CoverURL             string      `json:"cover_url,omitempty" doc:"Short-lived download URL (at most 5 minutes)"`
	CoverPublicURL       string      `json:"cover_public_url,omitempty" doc:"Permanent, cacheable URL of the cover (content addressed: a new cover has a new URL; immutable Cache-Control). Only for a published or completed public event, not of a sensitive category, of an active organiser that is not a sensitive club, once the virus scan cleared the image; absent otherwise (use cover_url)."`
	Visibility           string      `json:"visibility" enum:"public,university,members"`
	Status               string      `json:"status" enum:"draft,published,cancelled,completed"`
	RequiresRegistration bool        `json:"requires_registration"`
	RegistrationMode     string      `json:"registration_mode" enum:"auto,manual"`
	Capacity             *int        `json:"capacity,omitempty"`
	ShareableByOthers    bool        `json:"shareable_by_others" doc:"Whether an organisation other than the organiser may share the event in an event_share post (createPost): a published public event that is neither of a sensitive category nor organised by a sensitive club; false also while the organiser's sensitivity cannot be read. The organiser shares its own published public events whatever this says. Same rule as the 422 body.event_id of createPost (BE-133)."`
	Targetable           bool        `json:"targetable" doc:"Whether a company may name the event in an audience definition (definition.event of createSegment, countAudience and createTargetedPost; BE-161): a published or completed public event that is neither of a sensitive category nor organised by a sensitive club and started at most 365 days ago (or has yet to start); false also while the organiser's sensitivity cannot be read. Only registrants and attendees with a commercial message consent in force are ever counted or reached."`
}

// EventCardList is a page of event cards.
type EventCardList struct {
	Items      []EventCardDTO `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// EventDTO is the event page (getEvent and the writes). online_url is
// present for the event's managers and its confirmed attendees (03 §5.6),
// cover_file_id and is_sensitive_category for the managers only.
type EventDTO struct {
	EventCardDTO
	Description            map[string]any     `json:"description_json,omitempty" doc:"Tiptap JSON document"`
	Address                string             `json:"address,omitempty"`
	OnlineURL              string             `json:"online_url,omitempty" doc:"Managers and confirmed attendees only"`
	CoverFileID            string             `json:"cover_file_id,omitempty" doc:"Managers only"`
	VisibilityUniversityID string             `json:"visibility_university_id,omitempty"`
	PublishedAt            *time.Time         `json:"published_at,omitempty"`
	CancelledAt            *time.Time         `json:"cancelled_at,omitempty"`
	CancelReason           string             `json:"cancel_reason,omitempty"`
	ConfirmedCount         int                `json:"confirmed_count"`
	WaitlistEnabled        bool               `json:"waitlist_enabled"`
	RegistrationOpensAt    *time.Time         `json:"registration_opens_at,omitempty"`
	RegistrationClosesAt   *time.Time         `json:"registration_closes_at,omitempty"`
	CancelDeadlineAt       *time.Time         `json:"cancel_deadline_at,omitempty"`
	CheckinEnabled         bool               `json:"checkin_enabled"`
	IsSensitiveCategory    *bool              `json:"is_sensitive_category,omitempty" doc:"Managers only"`
	MyRegistration         *MyRegistrationDTO `json:"my_registration,omitempty" doc:"The caller's live registration (signed-in callers)"`
	CreatedAt              time.Time          `json:"created_at"`
	UpdatedAt              time.Time          `json:"updated_at"`
}

// MyRegistrationDTO is the caller's live registration on the event page.
type MyRegistrationDTO struct {
	RegistrationID string `json:"registration_id" format:"uuid"`
	Status         string `json:"status" enum:"pending,confirmed,waitlisted,checked_in,no_show"`
}

// StaffDTO is one staff member (04 §2.6, §E.4): last_name is the initial.
type StaffDTO struct {
	UserID    string `json:"user_id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name" doc:"Initial of the last name only (\"Y.\")"`
	Role      string `json:"role" enum:"scanner,manager"`
}

// StaffList is the staff of an event.
type StaffList struct {
	Items []StaffDTO `json:"items"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func toCardDTO(v EventView) EventCardDTO {
	e := v.Event
	return EventCardDTO{
		EventID: e.ID.String(),
		Org:     EventOrgDTO{OrgID: v.Org.ID.String(), Kind: string(v.Org.Kind), Name: v.Org.Name, Slug: v.Org.Slug},
		Title:   e.Title, Slug: e.Slug, Summary: str(e.Summary), Category: string(e.Category), Format: string(e.Format),
		LocationName: str(e.LocationName), City: str(e.City), StartsAt: e.StartsAt, EndsAt: e.EndsAt,
		DisplayTimezone: displayTimezone, CoverURL: v.CoverURL, CoverPublicURL: v.CoverPublicURL, Visibility: string(e.Visibility), Status: string(e.Status),
		RequiresRegistration: e.RequiresRegistration, RegistrationMode: string(e.RegistrationMode), Capacity: e.Capacity,
		ShareableByOthers: v.ShareableByOthers, Targetable: v.Targetable,
	}
}

func toEventDTO(v EventView) EventDTO {
	e := v.Event
	out := EventDTO{
		EventCardDTO: toCardDTO(v), Address: str(e.Address), PublishedAt: e.PublishedAt, CancelledAt: e.CancelledAt,
		CancelReason: str(e.CancelReason), ConfirmedCount: e.ConfirmedCount, WaitlistEnabled: e.WaitlistEnabled,
		RegistrationOpensAt: e.RegistrationOpensAt, RegistrationClosesAt: e.RegistrationClosesAt,
		CancelDeadlineAt: e.CancelDeadlineAt, CheckinEnabled: e.CheckinEnabled, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
	if e.VisibilityUniversityID != nil {
		out.VisibilityUniversityID = e.VisibilityUniversityID.String()
	}
	if len(e.DescriptionJSON) > 0 {
		var doc map[string]any
		if json.Unmarshal(e.DescriptionJSON, &doc) == nil {
			out.Description = doc
		}
	}
	if v.Manager || v.Attendee() {
		out.OnlineURL = str(e.OnlineURL)
	}
	if r := v.MyRegistration; r != nil {
		out.MyRegistration = &MyRegistrationDTO{RegistrationID: r.ID.String(), Status: string(r.Status)}
	}
	if v.Manager {
		if e.CoverFileID != nil {
			out.CoverFileID = e.CoverFileID.String()
		}
		out.IsSensitiveCategory = ptr(e.IsSensitiveCategory)
	}
	return out
}

func toStaffDTO(v StaffView) StaffDTO {
	return StaffDTO{UserID: v.UserID.String(), FirstName: v.FirstName, LastName: v.LastInitial, Role: string(v.Role)}
}
