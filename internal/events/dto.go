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
	Visibility           string      `json:"visibility" enum:"public,university,members"`
	Status               string      `json:"status" enum:"draft,published,cancelled,completed"`
	RequiresRegistration bool        `json:"requires_registration"`
	RegistrationMode     string      `json:"registration_mode" enum:"auto,manual"`
	Capacity             *int        `json:"capacity,omitempty"`
}

// EventCardList is a page of event cards.
type EventCardList struct {
	Items      []EventCardDTO `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// EventDTO is the event page (getEvent and the writes). online_url,
// cover_file_id and is_sensitive_category are present for the event's
// managers only (03 §5.6; approved attendees: BE-028).
type EventDTO struct {
	EventCardDTO
	Description            map[string]any `json:"description_json,omitempty" doc:"Tiptap JSON document"`
	Address                string         `json:"address,omitempty"`
	OnlineURL              string         `json:"online_url,omitempty" doc:"Managers only (approved attendees: registrations)"`
	CoverFileID            string         `json:"cover_file_id,omitempty" doc:"Managers only"`
	VisibilityUniversityID string         `json:"visibility_university_id,omitempty"`
	PublishedAt            *time.Time     `json:"published_at,omitempty"`
	CancelledAt            *time.Time     `json:"cancelled_at,omitempty"`
	CancelReason           string         `json:"cancel_reason,omitempty"`
	ConfirmedCount         int            `json:"confirmed_count"`
	WaitlistEnabled        bool           `json:"waitlist_enabled"`
	RegistrationOpensAt    *time.Time     `json:"registration_opens_at,omitempty"`
	RegistrationClosesAt   *time.Time     `json:"registration_closes_at,omitempty"`
	CancelDeadlineAt       *time.Time     `json:"cancel_deadline_at,omitempty"`
	CheckinEnabled         bool           `json:"checkin_enabled"`
	IsSensitiveCategory    *bool          `json:"is_sensitive_category,omitempty" doc:"Managers only"`
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
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
		DisplayTimezone: displayTimezone, CoverURL: v.CoverURL, Visibility: string(e.Visibility), Status: string(e.Status),
		RequiresRegistration: e.RequiresRegistration, RegistrationMode: string(e.RegistrationMode), Capacity: e.Capacity,
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
	if v.Manager {
		out.OnlineURL = str(e.OnlineURL)
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
