package events

import (
	"cmp"
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// errCheck mirrors a CHECK violation of 02 §11 (the GORM store gets 422
// from db.Translate).
var errCheck = problem.ErrValidation.Wrap(errors.New("events: check constraint violated"))

var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// MemStore is the in-memory Store of service, wiring and matrix tests
// (05 §13.2). It enforces the CHECK, unique and draft-only-delete rules of
// 02 §11.1–11.3 a test could otherwise meet only in PostgreSQL, and copies
// org_id from the events row like the GORM store (06 H-38). Every method is
// atomic under one mutex; it has no transactions (db.NopTx).
type MemStore struct {
	mu       sync.Mutex
	events   map[uuid.UUID]Event
	forms    map[uuid.UUID]Form         // by event id
	versions map[uuid.UUID]FormVersion  // by id; never changed once written
	rules    []LintRule                 // insertion order; seeded with DefaultLintRules
	reg      map[uuid.UUID]bool         // events marked as having a registration (MarkRegistered; form tests)
	staff    []Staff                    // insertion order
	regs     map[uuid.UUID]Registration // by id (BE-028)
	tickets  []Ticket                   // insertion order (BE-028)
	checkins []Checkin                  // insertion order (BE-031)
	// now stamps created_at/updated_at like the column defaults and the
	// touch trigger; updated_at always moves forward (two writes at the same
	// fake instant still change the ETag, as two transactions do).
	now func() time.Time
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory Store; now stamps the rows.
// The lint rules start as the seed of migration 000060 (DefaultLintRules).
func NewMemStore(now func() time.Time) *MemStore {
	s := &MemStore{
		events: map[uuid.UUID]Event{}, forms: map[uuid.UUID]Form{}, versions: map[uuid.UUID]FormVersion{},
		reg: map[uuid.UUID]bool{}, regs: map[uuid.UUID]Registration{}, now: now,
	}
	seeded := s.stamp(time.Time{})
	for _, r := range DefaultLintRules() {
		r.CreatedAt, r.UpdatedAt = seeded, seeded
		s.rules = append(s.rules, r)
	}
	return s
}

func (s *MemStore) stamp(prev time.Time) time.Time {
	t := s.now().UTC().Truncate(time.Microsecond)
	if !t.After(prev) {
		t = prev.Add(time.Microsecond)
	}
	return t
}

func runes(p *string) int {
	if p == nil {
		return 0
	}
	return utf8.RuneCountInString(*p)
}

// checkEvent applies the CHECK constraints of events (02 §11.1).
func checkEvent(e Event) error {
	n := utf8.RuneCountInString(e.Title)
	switch {
	case n < 3 || n > 160, !slugRe.MatchString(e.Slug), len(e.Slug) > 100, runes(e.Summary) > 300,
		runes(e.DescriptionText) > 20000, !e.Category.Valid(), !e.Format.Valid(), runes(e.LocationName) > 200,
		runes(e.Address) > 500, runes(e.City) > 60, !e.Visibility.Valid(), !e.RegistrationMode.Valid(),
		runes(e.CancelReason) > 500:
		return errCheck
	case e.OnlineURL != nil && (!strings.HasPrefix(*e.OnlineURL, "https://") || len(*e.OnlineURL) > 2048):
		return errCheck
	case !e.EndsAt.After(e.StartsAt):
		return errCheck
	case e.Capacity != nil && *e.Capacity <= 0, e.ConfirmedCount < 0,
		e.Capacity != nil && e.ConfirmedCount > *e.Capacity:
		return errCheck
	case (e.Visibility == VisibilityUniversity) != (e.VisibilityUniversityID != nil):
		return errCheck
	}
	switch e.Status {
	case StatusDraft, StatusPublished, StatusCancelled, StatusCompleted:
	default:
		return errCheck
	}
	switch {
	case e.Status != StatusDraft && e.PublishedAt == nil,
		(e.Status == StatusCancelled) != (e.CancelledAt != nil):
		return errCheck
	case e.RegistrationOpensAt != nil && e.RegistrationClosesAt != nil && !e.RegistrationClosesAt.After(*e.RegistrationOpensAt),
		e.RegistrationClosesAt != nil && e.RegistrationClosesAt.After(e.EndsAt):
		return errCheck
	case e.Format != FormatInPerson && e.Status != StatusDraft && e.OnlineURL == nil:
		return errCheck
	case !e.RequiresRegistration && (e.Capacity != nil || e.WaitlistEnabled || e.CheckinEnabled),
		e.WaitlistEnabled && e.Capacity == nil,
		e.AttendanceFinalizedAt != nil && !e.CheckinEnabled:
		return errCheck
	}
	return nil
}

// InsertEvent implements Store.
func (s *MemStore) InsertEvent(_ context.Context, e Event) (Event, bool, error) {
	if e.ID == uuid.Nil() || e.OrgID == uuid.Nil() || e.CreatedBy == uuid.Nil() {
		return Event{}, false, errors.New("events: insert event: missing id")
	}
	if err := checkEvent(e); err != nil {
		return Event{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.events[e.ID]; dup {
		return Event{}, false, problem.ErrConflict
	}
	for _, x := range s.events {
		if x.OrgID == e.OrgID && x.Slug == e.Slug {
			return Event{}, false, nil
		}
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = s.stamp(time.Time{})
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = e.CreatedAt
	}
	e.DescriptionJSON = cloneJSON(e.DescriptionJSON)
	s.events[e.ID] = e
	return e, true, nil
}

func cloneJSON(j JSON) JSON {
	if j == nil {
		return nil
	}
	return slices.Clone(j)
}

// GetEvent implements Store.
func (s *MemStore) GetEvent(_ context.Context, id uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[id]
	if !ok {
		return Event{}, ErrNotFound
	}
	e.DescriptionJSON = cloneJSON(e.DescriptionJSON)
	return e, nil
}

// LockEvent implements Store.
func (s *MemStore) LockEvent(ctx context.Context, id uuid.UUID) (Event, error) {
	return s.GetEvent(ctx, id)
}

// UpdateEvent implements Store.
func (s *MemStore) UpdateEvent(_ context.Context, e Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.events[e.ID]
	if !ok || cur.OrgID != e.OrgID {
		return Event{}, ErrNotFound
	}
	next := e
	next.CreatedBy, next.CreatedAt, next.ConfirmedCount = cur.CreatedBy, cur.CreatedAt, cur.ConfirmedCount
	next.AttendanceFinalizedAt, next.AttendanceFinalizedBy = cur.AttendanceFinalizedAt, cur.AttendanceFinalizedBy
	if err := checkEvent(next); err != nil {
		return Event{}, err
	}
	for _, x := range s.events {
		if x.ID != e.ID && x.OrgID == e.OrgID && x.Slug == e.Slug {
			return Event{}, ErrSlugTaken
		}
	}
	next.DescriptionJSON = cloneJSON(e.DescriptionJSON)
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.events[e.ID] = next
	return next, nil
}

// DeleteDraft implements Store.
func (s *MemStore) DeleteDraft(_ context.Context, orgID, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[id]
	if !ok || e.OrgID != orgID || e.Status != StatusDraft {
		return ErrNotFound
	}
	if f, ok := s.forms[id]; ok && f.CurrentVersionID != nil {
		return ErrInvalidState // a published version keeps its form (FK)
	}
	s.staff = slices.DeleteFunc(s.staff, func(x Staff) bool { return x.EventID == id })
	delete(s.forms, id)
	delete(s.events, id)
	return nil
}

// visibleTo is the store side of 03 §3.6 for a published event.
func (s *MemStore) visibleTo(v Viewer, e Event) bool {
	switch {
	case v.All, e.Visibility == VisibilityPublic:
		return true
	case e.Visibility == VisibilityUniversity && v.UniversityID != nil && e.VisibilityUniversityID != nil &&
		*v.UniversityID == *e.VisibilityUniversityID:
		return true
	case slices.Contains(v.MemberOrgs, e.OrgID):
		return true
	case v.UserID != nil:
		return slices.ContainsFunc(s.staff, func(x Staff) bool { return x.EventID == e.ID && x.UserID == *v.UserID })
	}
	return false
}

// memNorm approximates koro_norm for the in-memory search: Turkish lower
// case, ASCII transliteration, runs of other characters as one hyphen.
func memNorm(s string) string { return textx.Slug(s, 0) }

func memMatch(e Event, q string) bool {
	n := memNorm(q)
	if n == "" {
		return false
	}
	for _, p := range []*string{&e.Title, e.Summary, e.DescriptionText} {
		if p != nil && strings.Contains(memNorm(*p), n) {
			return true
		}
	}
	return false
}

// ListEvents implements Store.
func (s *MemStore) ListEvents(_ context.Context, f ListFilter, after *After, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, e := range s.events {
		switch {
		case e.Status != StatusPublished, !s.visibleTo(f.Viewer, e):
			continue
		case f.OrgIDs != nil && !slices.Contains(f.OrgIDs, e.OrgID),
			f.Category != "" && e.Category != f.Category,
			f.Format != "" && e.Format != f.Format,
			f.City != "" && (e.City == nil || memNorm(*e.City) != memNorm(f.City)),
			f.StartsFrom != nil && e.StartsAt.Before(*f.StartsFrom),
			f.StartsTo != nil && !e.StartsAt.Before(*f.StartsTo),
			f.EndsAfter != nil && !e.EndsAt.After(*f.EndsAfter),
			f.Q != "" && !memMatch(e, f.Q):
			continue
		}
		out = append(out, e)
	}
	if f.Sort == SortPublishedNew {
		key := func(e Event) time.Time { return *e.PublishedAt }
		slices.SortFunc(out, func(a, b Event) int {
			return -cmp.Or(key(a).Compare(key(b)), compareUUID(a.ID, b.ID))
		})
		if after != nil {
			out = slices.DeleteFunc(out, func(e Event) bool {
				return cmp.Or(key(e).Compare(after.At), compareUUID(e.ID, after.ID)) >= 0
			})
		}
	} else {
		sortByStart(out)
		out = afterStart(out, after)
	}
	return limitOf(out, limit), nil
}

// ListOrgEvents implements Store.
func (s *MemStore) ListOrgEvents(_ context.Context, orgID uuid.UUID, f OrgListFilter, after *After, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, e := range s.events {
		if e.OrgID == orgID && (len(f.Statuses) == 0 || slices.Contains(f.Statuses, e.Status)) {
			out = append(out, e)
		}
	}
	sortByStart(out)
	return limitOf(afterStart(out, after), limit), nil
}

func sortByStart(es []Event) {
	slices.SortFunc(es, func(a, b Event) int { return cmp.Or(a.StartsAt.Compare(b.StartsAt), compareUUID(a.ID, b.ID)) })
}

func afterStart(es []Event, after *After) []Event {
	if after == nil {
		return es
	}
	return slices.DeleteFunc(es, func(e Event) bool {
		return cmp.Or(e.StartsAt.Compare(after.At), compareUUID(e.ID, after.ID)) <= 0
	})
}

func limitOf(es []Event, limit int) []Event {
	if limit >= 0 && len(es) > limit {
		es = es[:limit]
	}
	for i := range es {
		es[i].DescriptionJSON = cloneJSON(es[i].DescriptionJSON)
	}
	if es == nil {
		return []Event{}
	}
	return es
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		if c := cmp.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return 0
}

// InsertForm implements Store.
func (s *MemStore) InsertForm(_ context.Context, orgID uuid.UUID, f Form) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[f.EventID]
	if !ok || e.OrgID != orgID {
		return ErrNotFound
	}
	if _, dup := s.forms[f.EventID]; dup {
		return problem.ErrConflict // event_forms_event_id_uq
	}
	f.OrgID = e.OrgID // 06 H-38
	f.DraftSchema = cloneJSON(f.DraftSchema)
	if f.CreatedAt.IsZero() {
		f.CreatedAt = s.stamp(time.Time{})
	}
	f.UpdatedAt = f.CreatedAt
	s.forms[f.EventID] = f
	return nil
}

// Form returns the form row of an event (tests and matrix fixtures).
func (s *MemStore) Form(eventID uuid.UUID) (Form, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.forms[eventID]
	return f, ok
}

// SetFormVersion publishes the empty form as version versionID of an
// event's form and points the form at it (tests and matrix fixtures that
// need "a published form" without going through publishEventForm).
func (s *MemStore) SetFormVersion(eventID, versionID uuid.UUID) error {
	return s.PutFormVersion(eventID, versionID, FormSchema{SchemaVersion: FormSchemaVersion, Fields: []FormField{}})
}

// PutFormVersion publishes schema (no lint findings) as version versionID
// of an event's form and points the form at it (tests and fixtures).
func (s *MemStore) PutFormVersion(eventID, versionID uuid.UUID, schema FormSchema) error {
	s.mu.Lock()
	f, ok := s.forms[eventID]
	s.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	v, err := s.insertVersion(f.OrgID, FormVersion{
		ID: versionID, FormID: f.ID, Schema: JSON(schema.Canonical()), SchemaSHA256: schema.SHA256(),
		FieldCount: len(schema.Fields), LintReport: LintReport{}.JSON(), PublishedBy: uuid.NewV7(), PublishedAt: s.now(),
	})
	if err != nil {
		return err
	}
	_, err = s.setCurrent(f.OrgID, f.ID, v.ID)
	return err
}

// PutFormDraft replaces the draft schema of an event's form as is, with
// updated_at at (matrix fixtures: a fixed ETag).
func (s *MemStore) PutFormDraft(eventID uuid.UUID, schema FormSchema, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.forms[eventID]
	if !ok {
		return ErrNotFound
	}
	f.DraftSchema, f.UpdatedAt = JSON(schema.Canonical()), at
	s.forms[eventID] = f
	return nil
}

// MarkRegistered records that an event has a registration (BE-028 writes
// them; tests stand in for it).
func (s *MemStore) MarkRegistered(eventID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reg[eventID] = true
}

// HasPublishedForm implements Store.
func (s *MemStore) HasPublishedForm(_ context.Context, orgID, id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.forms[id]
	return ok && f.OrgID == orgID && f.CurrentVersionID != nil, nil
}

func (s *MemStore) staffIndex(orgID, eventID, userID uuid.UUID) int {
	return slices.IndexFunc(s.staff, func(x Staff) bool {
		return x.OrgID == orgID && x.EventID == eventID && x.UserID == userID
	})
}

// StaffRole implements Store.
func (s *MemStore) StaffRole(_ context.Context, orgID, eventID, userID uuid.UUID) (StaffRole, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.staffIndex(orgID, eventID, userID); i >= 0 {
		return s.staff[i].Role, true, nil
	}
	return "", false, nil
}

// ListStaff implements Store.
func (s *MemStore) ListStaff(_ context.Context, orgID, eventID uuid.UUID) ([]Staff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Staff{}
	for _, x := range s.staff {
		if x.OrgID == orgID && x.EventID == eventID {
			out = append(out, x)
		}
	}
	return out, nil
}

// InsertStaff implements Store.
func (s *MemStore) InsertStaff(_ context.Context, st Staff) (Staff, error) {
	if !st.Role.Valid() {
		return Staff{}, errCheck
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[st.EventID]
	if !ok || e.OrgID != st.OrgID {
		return Staff{}, ErrNotFound
	}
	if slices.ContainsFunc(s.staff, func(x Staff) bool { return x.EventID == st.EventID && x.UserID == st.UserID }) {
		return Staff{}, ErrStaffExists
	}
	st.OrgID = e.OrgID // 06 H-38
	st.CreatedAt = s.stamp(time.Time{})
	s.staff = append(s.staff, st)
	return st, nil
}

// UpdateStaffRole implements Store.
func (s *MemStore) UpdateStaffRole(_ context.Context, orgID, eventID, userID uuid.UUID, role StaffRole) (Staff, error) {
	if !role.Valid() {
		return Staff{}, errCheck
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.staffIndex(orgID, eventID, userID)
	if i < 0 {
		return Staff{}, ErrNotFound
	}
	s.staff[i].Role = role
	return s.staff[i], nil
}

// DeleteStaff implements Store.
func (s *MemStore) DeleteStaff(_ context.Context, orgID, eventID, userID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.staffIndex(orgID, eventID, userID)
	if i < 0 {
		return ErrNotFound
	}
	s.staff = slices.Delete(s.staff, i, i+1)
	return nil
}

// StaffOfUser implements Store.
func (s *MemStore) StaffOfUser(_ context.Context, userID uuid.UUID) ([]Staff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Staff{}
	for _, x := range s.staff {
		if x.UserID == userID {
			out = append(out, x)
		}
	}
	return out, nil
}

// DeleteStaffOfUser implements Store.
func (s *MemStore) DeleteStaffOfUser(_ context.Context, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.staff)
	s.staff = slices.DeleteFunc(s.staff, func(x Staff) bool { return x.UserID == userID })
	return n - len(s.staff), nil
}

// PutEvent stores e as is (matrix fixtures: a fixed updated_at, so the ETag
// of an untouched fixture is a constant). The CHECKs still apply.
func (s *MemStore) PutEvent(e Event) error {
	if err := checkEvent(e); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[e.ID] = e
	return nil
}
