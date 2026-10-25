package events

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
)

// EventInput is the body of createEvent (04 §2.6 EventWrite). Zero optional
// fields take the column defaults: visibility public, registration
// required in auto mode, no capacity, no waiting list, no check-in.
type EventInput struct {
	Title                  string
	Slug                   string // derived from the title when empty
	Summary                string
	Description            map[string]any // Tiptap JSON (05 §14.6); nil: none
	Category               Category
	Format                 Format
	LocationName           string
	Address                string
	City                   string
	OnlineURL              string
	StartsAt               time.Time
	EndsAt                 time.Time
	CoverFileID            *uuid.UUID
	Visibility             Visibility
	VisibilityUniversityID *uuid.UUID
	RequiresRegistration   *bool
	RegistrationMode       RegistrationMode
	Capacity               *int
	WaitlistEnabled        bool
	RegistrationOpensAt    *time.Time
	RegistrationClosesAt   *time.Time
	CancelDeadlineAt       *time.Time
	CheckinEnabled         bool
	IsSensitiveCategory    bool
}

// patch returns the input as a patch over the defaults of a new event.
func (in EventInput) patch() EventPatch {
	p := EventPatch{
		Title: &in.Title, Description: in.Description, Category: &in.Category, Format: &in.Format,
		StartsAt: &in.StartsAt, EndsAt: &in.EndsAt, CoverFileID: in.CoverFileID,
		VisibilityUniversityID: in.VisibilityUniversityID, RequiresRegistration: in.RequiresRegistration,
		Capacity: in.Capacity, WaitlistEnabled: &in.WaitlistEnabled, RegistrationOpensAt: in.RegistrationOpensAt,
		RegistrationClosesAt: in.RegistrationClosesAt, CancelDeadlineAt: in.CancelDeadlineAt,
		CheckinEnabled: &in.CheckinEnabled, IsSensitiveCategory: &in.IsSensitiveCategory,
	}
	for _, f := range []struct {
		v   string
		dst **string
	}{
		{in.Slug, &p.Slug}, {in.Summary, &p.Summary}, {in.LocationName, &p.LocationName}, {in.Address, &p.Address},
		{in.City, &p.City}, {in.OnlineURL, &p.OnlineURL},
	} {
		if f.v != "" {
			*f.dst = ptr(f.v)
		}
	}
	if in.Visibility != "" {
		p.Visibility = &in.Visibility
	}
	if in.RegistrationMode != "" {
		p.RegistrationMode = &in.RegistrationMode
	}
	return p
}

// EventPatch is the body of updateEvent (04 §2.6 "EventWrite kısmi"): nil
// leaves a field as it is; an empty string clears an optional text; Clear
// names the optional fields to empty (clearableFields).
type EventPatch struct {
	Title                  *string
	Slug                   *string
	Summary                *string
	Description            map[string]any
	Category               *Category
	Format                 *Format
	LocationName           *string
	Address                *string
	City                   *string
	OnlineURL              *string
	StartsAt               *time.Time
	EndsAt                 *time.Time
	CoverFileID            *uuid.UUID
	Visibility             *Visibility
	VisibilityUniversityID *uuid.UUID
	RequiresRegistration   *bool
	RegistrationMode       *RegistrationMode
	Capacity               *int
	WaitlistEnabled        *bool
	RegistrationOpensAt    *time.Time
	RegistrationClosesAt   *time.Time
	CancelDeadlineAt       *time.Time
	CheckinEnabled         *bool
	IsSensitiveCategory    *bool
	Clear                  []string
}

// clearableFields are the optional fields a patch may empty (Clear).
var clearableFields = []string{
	"summary", "description_json", "location_name", "address", "city", "online_url", "cover_file_id", "capacity",
	"registration_opens_at", "registration_closes_at", "cancel_deadline_at",
}

// ClearableFields lists the names of EventPatch.Clear (OpenAPI enum).
func ClearableFields() []string { return slices.Clone(clearableFields) }

func (p EventPatch) empty() bool {
	return p.Title == nil && p.Slug == nil && p.Summary == nil && p.Description == nil && p.Category == nil &&
		p.Format == nil && p.LocationName == nil && p.Address == nil && p.City == nil && p.OnlineURL == nil &&
		p.StartsAt == nil && p.EndsAt == nil && p.CoverFileID == nil && p.Visibility == nil &&
		p.VisibilityUniversityID == nil && p.RequiresRegistration == nil && p.RegistrationMode == nil &&
		p.Capacity == nil && p.WaitlistEnabled == nil && p.RegistrationOpensAt == nil && p.RegistrationClosesAt == nil &&
		p.CancelDeadlineAt == nil && p.CheckinEnabled == nil && p.IsSensitiveCategory == nil && len(p.Clear) == 0
}

// scheduleFields are the fields whose change on a published event is
// event.schedule_changed (01 §6.2: attendees get an updated .ics): the time
// and the venue.
var scheduleFields = []string{"starts_at", "ends_at", "format", "location_name", "address", "city", "online_url"}

// applied is the outcome of a patch: the new row, the names of the changed
// fields (audit metadata: names only, no values) and a new cover to attach.
type applied struct {
	next    Event
	changed []string
	cover   *uuid.UUID
}

// apply validates p field by field over e and then the whole row against
// the rules of 02 §11.1 (validateRow). It writes nothing.
func (s *service) apply(ctx context.Context, a authz.Actor, o orgs.OrgSummary, e Event, p EventPatch) (applied, error) {
	clear := map[string]bool{}
	for _, f := range p.Clear {
		if !slices.Contains(clearableFields, f) {
			return applied{}, problem.Field("body.clear", "unknown field "+f)
		}
		clear[f] = true
	}
	next := e
	set := func(loc string, v *string, maxRunes int, dst **string) error {
		if clear[loc] {
			if v != nil {
				return problem.Field("body."+loc, "is both set and cleared")
			}
			*dst = nil
			return nil
		}
		if v == nil {
			return nil
		}
		c, err := cleanOptional("body."+loc, *v, maxRunes)
		if err != nil {
			return err
		}
		*dst = c
		return nil
	}
	if p.Title != nil {
		t, err := cleanText("body.title", *p.Title, minTitle, maxTitle)
		if err != nil {
			return applied{}, err
		}
		next.Title = t
	}
	if p.Slug != nil {
		sl, err := cleanSlug("body.slug", *p.Slug)
		if err != nil {
			return applied{}, err
		}
		next.Slug = sl
	}
	for _, f := range []struct {
		loc string
		v   *string
		max int
		dst **string
	}{
		{"summary", p.Summary, maxSummary, &next.Summary}, {"location_name", p.LocationName, maxLocation, &next.LocationName},
		{"address", p.Address, maxAddress, &next.Address}, {"city", p.City, maxCity, &next.City},
	} {
		if err := set(f.loc, f.v, f.max, f.dst); err != nil {
			return applied{}, err
		}
	}
	switch {
	case clear["online_url"] && p.OnlineURL != nil:
		return applied{}, problem.Field("body.online_url", "is both set and cleared")
	case clear["online_url"]:
		next.OnlineURL = nil
	case p.OnlineURL != nil:
		u, err := cleanOnlineURL("body.online_url", *p.OnlineURL)
		if err != nil {
			return applied{}, err
		}
		next.OnlineURL = u
	}
	switch {
	case clear["description_json"] && p.Description != nil:
		return applied{}, problem.Field("body.description_json", "is both set and cleared")
	case clear["description_json"]:
		next.DescriptionJSON, next.DescriptionText = nil, nil
	case p.Description != nil:
		raw, err := json.Marshal(p.Description)
		if err != nil {
			return applied{}, problem.Field("body.description_json", "must be a Tiptap document")
		}
		doc, text, err := sanitizeDescription(raw)
		if err != nil {
			return applied{}, problem.Field("body.description_json",
				"must be a Tiptap document of paragraphs, headings (2-3), lists, quotes, bold, italic and https links (at most 20000 characters)")
		}
		next.DescriptionJSON, next.DescriptionText = doc, nil
		if text != "" {
			next.DescriptionText = &text
		}
	}
	if p.Category != nil {
		if !p.Category.Valid() {
			return applied{}, problem.Field("body.category", "unknown category")
		}
		next.Category = *p.Category
	}
	if p.Format != nil {
		if !p.Format.Valid() {
			return applied{}, problem.Field("body.format", "must be in_person, online or hybrid")
		}
		next.Format = *p.Format
	}
	if p.StartsAt != nil {
		if p.StartsAt.IsZero() {
			return applied{}, problem.Field("body.starts_at", "is required")
		}
		next.StartsAt = p.StartsAt.UTC().Truncate(time.Microsecond)
	}
	if p.EndsAt != nil {
		if p.EndsAt.IsZero() {
			return applied{}, problem.Field("body.ends_at", "is required")
		}
		next.EndsAt = p.EndsAt.UTC().Truncate(time.Microsecond)
	}
	if p.Visibility != nil {
		if !p.Visibility.Valid() {
			return applied{}, problem.Field("body.visibility", "must be public, university or members")
		}
		next.Visibility = *p.Visibility
	}
	if p.VisibilityUniversityID != nil {
		next.VisibilityUniversityID = ptr(*p.VisibilityUniversityID)
	}
	if next.Visibility != VisibilityUniversity {
		if p.VisibilityUniversityID != nil {
			return applied{}, problem.Field("body.visibility_university_id", "applies to university visibility only")
		}
		next.VisibilityUniversityID = nil
	}
	if p.RequiresRegistration != nil {
		next.RequiresRegistration = *p.RequiresRegistration
	}
	if p.RegistrationMode != nil {
		if !p.RegistrationMode.Valid() {
			return applied{}, problem.Field("body.registration_mode", "must be auto or manual")
		}
		next.RegistrationMode = *p.RegistrationMode
	}
	switch {
	case clear["capacity"] && p.Capacity != nil:
		return applied{}, problem.Field("body.capacity", "is both set and cleared")
	case clear["capacity"]:
		next.Capacity = nil
	case p.Capacity != nil:
		if *p.Capacity < 1 || *p.Capacity > maxCapacity {
			return applied{}, problem.Field("body.capacity", "must be 1-100000")
		}
		next.Capacity = ptr(*p.Capacity)
	}
	for _, f := range []struct {
		loc string
		v   *time.Time
		dst **time.Time
	}{
		{"registration_opens_at", p.RegistrationOpensAt, &next.RegistrationOpensAt},
		{"registration_closes_at", p.RegistrationClosesAt, &next.RegistrationClosesAt},
		{"cancel_deadline_at", p.CancelDeadlineAt, &next.CancelDeadlineAt},
	} {
		switch {
		case clear[f.loc] && f.v != nil:
			return applied{}, problem.Field("body."+f.loc, "is both set and cleared")
		case clear[f.loc]:
			*f.dst = nil
		case f.v != nil:
			*f.dst = ptr(f.v.UTC().Truncate(time.Microsecond))
		}
	}
	if p.WaitlistEnabled != nil {
		next.WaitlistEnabled = *p.WaitlistEnabled
	}
	if p.CheckinEnabled != nil {
		next.CheckinEnabled = *p.CheckinEnabled
	}
	if p.IsSensitiveCategory != nil {
		next.IsSensitiveCategory = *p.IsSensitiveCategory
	}
	out := applied{next: next}
	switch {
	case clear["cover_file_id"] && p.CoverFileID != nil:
		return applied{}, problem.Field("body.cover_file_id", "is both set and cleared")
	case clear["cover_file_id"]:
		out.next.CoverFileID = nil
	case p.CoverFileID != nil && (e.CoverFileID == nil || *e.CoverFileID != *p.CoverFileID):
		if err := s.checkCover(ctx, a, o, *p.CoverFileID); err != nil {
			return applied{}, err
		}
		out.next.CoverFileID, out.cover = ptr(*p.CoverFileID), ptr(*p.CoverFileID)
	}
	if err := s.validateRow(ctx, o, out.next); err != nil {
		return applied{}, err
	}
	out.changed = changedFields(e, out.next)
	return out, nil
}

// checkCover accepts an event_cover upload of the organiser or of the
// caller (files purpose rules, 01 §4.2).
func (s *service) checkCover(ctx context.Context, a authz.Actor, o orgs.OrgSummary, fileID uuid.UUID) error {
	if s.d.Files == nil {
		return problem.ErrUnavailable
	}
	bad := problem.Field("body.cover_file_id", "must be an event_cover upload of this organization")
	f, err := s.d.Files.Get(ctx, fileID)
	switch {
	case errors.Is(err, files.ErrNotFound):
		return bad
	case err != nil:
		return err
	}
	ownOrg := f.OrgID != nil && *f.OrgID == o.ID
	ownUpload := f.UploadedBy != nil && a.IsUser() && *f.UploadedBy == a.UserID
	if f.Purpose != files.PurposeEventCover || !ownOrg && !ownUpload {
		return bad
	}
	return nil
}

// validateRow applies the cross-field rules of 02 §11.1 (the CHECKs, so
// the caller gets a field error instead of a constraint name) and the
// university of a university-only event.
func (s *service) validateRow(ctx context.Context, o orgs.OrgSummary, e Event) error {
	switch {
	case !e.EndsAt.After(e.StartsAt):
		return problem.Field("body.ends_at", "must be after starts_at")
	case e.RegistrationOpensAt != nil && e.RegistrationClosesAt != nil && !e.RegistrationClosesAt.After(*e.RegistrationOpensAt):
		return problem.Field("body.registration_closes_at", "must be after registration_opens_at")
	case e.RegistrationClosesAt != nil && e.RegistrationClosesAt.After(e.EndsAt):
		return problem.Field("body.registration_closes_at", "must not be after ends_at")
	case e.CancelDeadlineAt != nil && e.CancelDeadlineAt.After(e.EndsAt):
		return problem.Field("body.cancel_deadline_at", "must not be after ends_at")
	case !e.RequiresRegistration && (e.Capacity != nil || e.WaitlistEnabled || e.CheckinEnabled):
		return problem.Field("body.requires_registration", "capacity, waiting list and check-in need registration")
	case e.WaitlistEnabled && e.Capacity == nil:
		return problem.Field("body.waitlist_enabled", "a waiting list needs a capacity")
	case e.Capacity != nil && *e.Capacity < e.ConfirmedCount:
		return problem.Field("body.capacity", "is below the confirmed registrations")
	case e.Format == FormatInPerson && e.OnlineURL != nil:
		return problem.Field("body.online_url", "applies to online and hybrid events only")
	case e.Format != FormatInPerson && e.Status != StatusDraft && e.OnlineURL == nil:
		return problem.Field("body.online_url", "is required for a published online or hybrid event")
	}
	if e.Visibility != VisibilityUniversity {
		return nil
	}
	if e.VisibilityUniversityID == nil {
		return problem.Field("body.visibility_university_id", "is required for university visibility")
	}
	return s.checkUniversity(ctx, o, *e.VisibilityUniversityID)
}

// checkUniversity: a university-only event is for an active university; a
// club targets its own university, a university org itself; the platform
// org any.
func (s *service) checkUniversity(ctx context.Context, o orgs.OrgSummary, id uuid.UUID) error {
	bad := problem.Field("body.visibility_university_id", "must be the organizer's own active university")
	u, err := s.d.Catalog.GetUniversity(ctx, id)
	switch {
	case errors.Is(err, directory.ErrNotFound):
		return bad
	case err != nil:
		return err
	case !u.IsActive:
		return bad
	}
	switch o.Kind {
	case orgs.KindUniversity:
		if u.OrgID != o.ID {
			return bad
		}
	case orgs.KindClub:
		c, err := s.d.Catalog.GetClub(ctx, o.ID)
		switch {
		case errors.Is(err, directory.ErrNotFound):
			return bad
		case err != nil:
			return err
		case c.UniversityID != id:
			return bad
		}
	}
	return nil
}

// changedFields names the columns that differ between a and b (audit
// metadata and event.schedule_changed).
func changedFields(a, b Event) []string {
	var out []string
	add := func(name string, differ bool) {
		if differ {
			out = append(out, name)
		}
	}
	add("title", a.Title != b.Title)
	add("slug", a.Slug != b.Slug)
	add("summary", !eqPtr(a.Summary, b.Summary))
	add("description_json", string(a.DescriptionJSON) != string(b.DescriptionJSON))
	add("category", a.Category != b.Category)
	add("format", a.Format != b.Format)
	add("location_name", !eqPtr(a.LocationName, b.LocationName))
	add("address", !eqPtr(a.Address, b.Address))
	add("city", !eqPtr(a.City, b.City))
	add("online_url", !eqPtr(a.OnlineURL, b.OnlineURL))
	add("starts_at", !a.StartsAt.Equal(b.StartsAt))
	add("ends_at", !a.EndsAt.Equal(b.EndsAt))
	add("cover_file_id", !eqPtr(a.CoverFileID, b.CoverFileID))
	add("visibility", a.Visibility != b.Visibility)
	add("visibility_university_id", !eqPtr(a.VisibilityUniversityID, b.VisibilityUniversityID))
	add("requires_registration", a.RequiresRegistration != b.RequiresRegistration)
	add("registration_mode", a.RegistrationMode != b.RegistrationMode)
	add("capacity", !eqPtr(a.Capacity, b.Capacity))
	add("waitlist_enabled", a.WaitlistEnabled != b.WaitlistEnabled)
	add("registration_opens_at", !eqTime(a.RegistrationOpensAt, b.RegistrationOpensAt))
	add("registration_closes_at", !eqTime(a.RegistrationClosesAt, b.RegistrationClosesAt))
	add("cancel_deadline_at", !eqTime(a.CancelDeadlineAt, b.CancelDeadlineAt))
	add("checkin_enabled", a.CheckinEnabled != b.CheckinEnabled)
	add("is_sensitive_category", a.IsSensitiveCategory != b.IsSensitiveCategory)
	return out
}

func eqPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func eqTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
