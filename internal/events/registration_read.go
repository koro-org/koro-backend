package events

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/page"
	"koro/internal/students"
)

// ListMyRegistrations implements Service: the caller's own registrations
// (any org), by the event's start; upcoming keeps events that have not
// ended. No answers in the list.
func (s *service) ListMyRegistrations(ctx context.Context, a authz.Actor, q MyRegistrationQuery) (RegistrationPage, error) {
	if !a.IsUser() {
		return RegistrationPage{}, authz.ErrForbidden
	}
	me := a.UserID
	if err := authz.Can(ctx, a, authz.EventsRegistrationListOwn, authz.Resource{Type: authz.TypeRegistration,
		OwnerUserID: &me, Attrs: map[string]string{authz.AttrStatus: "", authz.AttrEventStaffRole: "none"}}); err != nil {
		return RegistrationPage{}, err
	}
	f := MyRegFilter{Statuses: q.Statuses}
	if q.Upcoming {
		now := s.now()
		f.EndsAfter = &now
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListUserRegistrations(ctx, me, f, q.After, limit+1)
	if err != nil {
		return RegistrationPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	evs, err := s.d.Store.GetEvents(ctx, eventIDs(rows))
	if err != nil {
		return RegistrationPage{}, err
	}
	out := RegistrationPage{Items: make([]RegistrationView, 0, len(rows))}
	for _, r := range rows {
		e, ok := evs[r.EventID]
		if !ok {
			continue // an event row never disappears once it has registrations
		}
		v, err := s.selfView(ctx, r, e)
		if err != nil {
			return RegistrationPage{}, err
		}
		out.Items = append(out.Items, v)
	}
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		out.Next = &After{At: evs[last.EventID].StartsAt, ID: last.ID}
	}
	return out, nil
}

// GetMyRegistration implements Service: the caller's own registration
// with its answers; the sensitive answers are opened for the owner and
// the read is audited (02 §11.6: "her okuma audit").
func (s *service) GetMyRegistration(ctx context.Context, a authz.Actor, id uuid.UUID) (RegistrationView, error) {
	if !a.IsUser() {
		return RegistrationView{}, authz.ErrForbidden
	}
	r, err := s.d.Store.GetUserRegistration(ctx, a.UserID, id)
	if err != nil {
		return RegistrationView{}, err
	}
	if err := authz.Can(ctx, a, authz.EventsRegistrationReadOwn, ownResource(r)); err != nil {
		return RegistrationView{}, err
	}
	v, err := s.ownView(ctx, a.UserID, id, true)
	if err != nil {
		return RegistrationView{}, err
	}
	if v.AnsweredForm, err = s.answeredForm(ctx, a, r); err != nil {
		return RegistrationView{}, err
	}
	if len(v.AnswersSensitive) > 0 {
		meta := map[string]any{"event_id": r.EventID.String(), "sensitive_fields": keysOf(v.AnswersSensitive)}
		if err := s.logRead(ctx, auditEvent(a, authz.EventsRegistrationReadOwn, regAggType, r.ID, r.OrgID, "", meta)); err != nil {
			return RegistrationView{}, err
		}
	}
	return v, nil
}

// answeredForm returns the form version r was answered against (nil for a
// registration without a form), as its registrant read it: a minor never
// saw the minor-restricted consent fields (ForReader). The version comes
// through r's org (06 H-38); versions are immutable, so the labels are the
// ones the registrant answered.
func (s *service) answeredForm(ctx context.Context, a authz.Actor, r Registration) (*AnsweredForm, error) {
	if r.FormVersionID == nil {
		return nil, nil
	}
	v, err := s.d.Store.GetFormVersion(ctx, r.OrgID, *r.FormVersionID)
	if err != nil {
		return nil, err
	}
	schema, err := DecodeFormSchema(v.Schema)
	if err != nil {
		return nil, err
	}
	return &AnsweredForm{VersionNo: v.VersionNo, Schema: schema.ForReader(a.IsMinor)}, nil
}

// ownView reads registration id of userID with its event; withAnswers
// adds the answers (the sensitive ones opened).
func (s *service) ownView(ctx context.Context, userID, id uuid.UUID, withAnswers bool) (RegistrationView, error) {
	r, err := s.d.Store.GetUserRegistration(ctx, userID, id)
	if err != nil {
		return RegistrationView{}, err
	}
	e, err := s.d.Store.GetEvent(ctx, r.EventID)
	if err != nil {
		return RegistrationView{}, err
	}
	v, err := s.selfView(ctx, r, e)
	if err != nil || !withAnswers {
		return v, err
	}
	v.Answers = decodeAnswers(r.Answers)
	if v.AnswersSensitive, err = s.openSensitive(ctx, r); err != nil {
		return RegistrationView{}, err
	}
	return v, nil
}

// selfView is the registrant's view of r (no answers): the waiting list
// place and whether a ticket is ready.
func (s *service) selfView(ctx context.Context, r Registration, e Event) (RegistrationView, error) {
	v := RegistrationView{Registration: r, Event: e, ConsentSnapshot: decodeSnapshot(r.ConsentSnapshot)}
	switch {
	case r.Status == RegWaitlisted:
		n, err := s.d.Store.WaitlistPosition(ctx, r.OrgID, r.EventID, r.ID)
		if err != nil {
			return RegistrationView{}, err
		}
		v.WaitlistPosition = &n
	case r.Status.Counted():
		_, ok, err := s.d.Store.ActiveTicket(ctx, r.OrgID, r.ID)
		if err != nil {
			return RegistrationView{}, err
		}
		v.TicketAvailable = ok
	}
	return v, nil
}

// registrationSurface is the event's resource for the registration actions of its
// organiser (the registration list and each registration are the event's
// sub-surface, 03 §7.6), with the break-glass reason (03 §6.3).
func (s *service) registrationSurface(ctx context.Context, a authz.Actor, e Event, access Access) (authz.Resource, error) {
	res, err := s.resource(ctx, a, e)
	if err != nil {
		return authz.Resource{}, err
	}
	if access.Reason != "" {
		res.Attrs[authz.AttrAccessReason] = access.Reason
	}
	return res, nil
}

// breakGlass reports whether a reads the registrations of e as platform
// staff (support, platform_admin outside the platform org): B (03 §6.3).
func breakGlass(a authz.Actor, e Event) bool {
	return a.HasAnyPlatformRole() && !a.MemberOf(e.OrgID)
}

// ListEventRegistrations implements Service. Order: the event (404) →
// authz on its registration surface (404 invisible; 403 without the role;
// 403 authz.break_glass_reason_required for support/platform_admin
// without a reason; 404 by break-glass on a sensitive event, Y2) → the
// page → the audit record (every list, 03 §6.5; break-glass with the
// reason). The sensitive answers are never in a list (k14).
func (s *service) ListEventRegistrations(ctx context.Context, a authz.Actor, eventID uuid.UUID, q EventRegistrationQuery, access Access) (RegistrationPage, error) {
	e, err := s.d.Store.GetEvent(ctx, eventID)
	if err != nil {
		return RegistrationPage{}, err
	}
	res, err := s.registrationSurface(ctx, a, e, access)
	if err != nil {
		return RegistrationPage{}, err
	}
	if err := authz.Can(ctx, a, authz.EventsRegistrationList, res); err != nil {
		return RegistrationPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListEventRegistrations(ctx, e.OrgID, e.ID, RegFilter{Statuses: q.Statuses, Desc: q.Desc}, q.After, limit+1)
	if err != nil {
		return RegistrationPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	bg := breakGlass(a, e)
	meta, reason := map[string]any{"count": len(rows)}, ""
	if bg {
		meta[audit.MetaBreakGlass], reason = true, access.Reason
		if access.TicketRef != "" {
			meta[audit.MetaTicketRef] = access.TicketRef
		}
	}
	if err := s.logRead(ctx, auditEvent(a, authz.EventsRegistrationList, eventAggType, e.ID, e.OrgID, reason, meta)); err != nil {
		return RegistrationPage{}, err
	}
	items, err := s.orgViews(ctx, e, rows, !bg)
	if err != nil {
		return RegistrationPage{}, err
	}
	out := RegistrationPage{Items: items}
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		out.Next = &After{At: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}

// GetEventRegistration implements Service. Order as the list, then the
// registration of this event (404). The organiser's managers get the
// sensitive answers opened; a break-glass read never does (03 §6.3), and
// its audit record names the registrant (notify tells them). Every read is
// audited with the keys of the sensitive fields read (04 §2.6).
func (s *service) GetEventRegistration(ctx context.Context, a authz.Actor, eventID, id uuid.UUID, access Access) (RegistrationView, error) {
	e, err := s.d.Store.GetEvent(ctx, eventID)
	if err != nil {
		return RegistrationView{}, err
	}
	res, err := s.registrationSurface(ctx, a, e, access)
	if err != nil {
		return RegistrationView{}, err
	}
	if err := authz.Can(ctx, a, authz.EventsRegistrationRead, res); err != nil {
		return RegistrationView{}, err
	}
	r, err := s.d.Store.GetRegistration(ctx, e.OrgID, e.ID, id)
	if err != nil {
		return RegistrationView{}, err
	}
	bg := breakGlass(a, e)
	vs, err := s.orgViews(ctx, e, []Registration{r}, !bg)
	if err != nil {
		return RegistrationView{}, err
	}
	v := vs[0]
	if !bg {
		if v.AnswersSensitive, err = s.openSensitive(ctx, r); err != nil {
			return RegistrationView{}, err
		}
	}
	meta, reason := map[string]any{"event_id": e.ID.String(), "sensitive_fields": keysOf(v.AnswersSensitive)}, ""
	if bg {
		meta[audit.MetaBreakGlass], meta[audit.MetaSubjectUserID], reason = true, r.UserID.String(), access.Reason
		if access.TicketRef != "" {
			meta[audit.MetaTicketRef] = access.TicketRef
		}
	}
	if err := s.logRead(ctx, auditEvent(a, authz.EventsRegistrationRead, regAggType, r.ID, e.OrgID, reason, meta)); err != nil {
		return RegistrationView{}, err
	}
	return v, nil
}

// logRead writes the audit record of an always-audited read in its own
// transaction (03 §6.5).
func (s *service) logRead(ctx context.Context, e audit.Event) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error { return s.d.Audit.Log(ctx, e) })
}

// orgViews builds the organiser's views of rows of e: the stored answers
// (never the sensitive ones), the consent snapshot, the registrant card
// (04 §V.6) and, for the managers, the decision note.
func (s *service) orgViews(ctx context.Context, e Event, rows []Registration, showNote bool) ([]RegistrationView, error) {
	cards, err := s.registrants(ctx, rows)
	if err != nil {
		return nil, err
	}
	out := make([]RegistrationView, len(rows))
	for i, r := range rows {
		card := cards[r.UserID]
		out[i] = RegistrationView{
			Registration: r, Event: e, Answers: decodeAnswers(r.Answers), ConsentSnapshot: decodeSnapshot(r.ConsentSnapshot),
			User: &card, ShowNote: showNote,
		}
	}
	return out, nil
}

// registrants returns the cards of the registrants of rows: names
// (identity), avatar, university, department and class year (students,
// directory). A missing piece is left empty; a read never fails on it.
func (s *service) registrants(ctx context.Context, rows []Registration) (map[uuid.UUID]Registrant, error) {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if !slices.Contains(ids, r.UserID) {
			ids = append(ids, r.UserID)
		}
	}
	out := make(map[uuid.UUID]Registrant, len(ids))
	for _, id := range ids {
		out[id] = Registrant{UserID: id}
	}
	if len(ids) == 0 {
		return out, nil
	}
	if s.d.Users != nil {
		us, err := s.d.Users.GetMany(ctx, ids)
		if err != nil {
			return nil, err
		}
		for id, u := range us {
			c := out[id]
			c.FirstName, c.LastName = u.FirstName, u.LastName
			out[id] = c
		}
	}
	if s.d.Students == nil {
		return out, nil
	}
	avatars, err := s.d.Students.AvatarURLs(ctx, ids)
	if err != nil {
		return nil, err
	}
	unis, deps := map[uuid.UUID]string{}, map[uuid.UUID]string{}
	for _, id := range ids {
		c := out[id]
		c.AvatarURL = avatars[id]
		p, err := s.d.Students.AudienceProfile(ctx, id)
		switch {
		case errors.Is(err, students.ErrNotFound):
			out[id] = c
			continue
		case err != nil:
			return nil, err
		}
		c.ClassYear = p.ClassYear
		c.UniversityName = s.universityName(ctx, unis, p.UniversityID)
		if p.DepartmentID != nil {
			c.DepartmentName = s.departmentName(ctx, deps, *p.DepartmentID)
		}
		out[id] = c
	}
	return out, nil
}

func (s *service) universityName(ctx context.Context, cache map[uuid.UUID]string, id uuid.UUID) string {
	if n, ok := cache[id]; ok {
		return n
	}
	n := ""
	if u, err := s.d.Catalog.GetUniversity(ctx, id); err == nil {
		n = u.Name
	}
	cache[id] = n
	return n
}

func (s *service) departmentName(ctx context.Context, cache map[uuid.UUID]string, id uuid.UUID) string {
	if n, ok := cache[id]; ok {
		return n
	}
	n := ""
	if d, err := s.d.Catalog.GetDepartment(ctx, id); err == nil {
		n = d.Name
	}
	cache[id] = n
	return n
}

// openSensitive opens the warn_special answers of r (nil when there are
// none or they were purged). Without a sealer they stay closed (logged).
func (s *service) openSensitive(ctx context.Context, r Registration) (map[string]any, error) {
	if len(r.AnswersSensitiveEnc) == 0 {
		return nil, nil
	}
	if s.d.Sealer == nil {
		s.d.Log.WarnContext(ctx, "events: sensitive answers cannot be opened: no sealer", slog.String("registration_id", r.ID.String()))
		return nil, nil
	}
	b, err := s.d.Sealer.Open(ctx, SensitiveAnswersPurpose(r.ID), r.AnswersSensitiveEnc)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, errors.New("events: sensitive answers are not a JSON object")
	}
	return out, nil
}

// CountInOrg implements Registrations.
func (s *service) CountInOrg(ctx context.Context, orgID, userID uuid.UUID) (int, error) {
	return s.d.Store.CountUserRegistrationsInOrg(ctx, orgID, userID)
}

// SummaryOf implements Registrations: newest first, sensitive events left
// out (the admin view is a break-glass read, 03 Y2).
func (s *service) SummaryOf(ctx context.Context, userID uuid.UUID) ([]RegistrationSummary, error) {
	rows, err := s.d.Store.RegistrationsOfUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	evs, err := s.d.Store.GetEvents(ctx, eventIDs(rows))
	if err != nil {
		return nil, err
	}
	orgIDs := []uuid.UUID{}
	for _, e := range evs {
		if !slices.Contains(orgIDs, e.OrgID) {
			orgIDs = append(orgIDs, e.OrgID)
		}
	}
	sensClubs, err := s.d.Catalog.SensitiveOrgIDs(ctx, orgIDs)
	if err != nil {
		return nil, err
	}
	out := []RegistrationSummary{}
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		e, ok := evs[r.EventID]
		if !ok || e.IsSensitiveCategory || sensClubs[e.OrgID] {
			continue
		}
		out = append(out, RegistrationSummary{RegistrationID: r.ID, EventID: e.ID, EventTitle: e.Title, Status: r.Status,
			RegisteredAt: r.CreatedAt})
	}
	return out, nil
}

// myRegistration is the caller's live registration of e (getEvent's
// my_registration; a confirmed one shows online_url).
func (s *service) myRegistration(ctx context.Context, a authz.Actor, e Event) (*Registration, error) {
	if !a.IsUser() {
		return nil, nil
	}
	r, ok, err := s.d.Store.ActiveRegistration(ctx, e.OrgID, e.ID, a.UserID)
	if err != nil || !ok {
		return nil, err
	}
	return &r, nil
}

func eventIDs(rows []Registration) []uuid.UUID {
	out := []uuid.UUID{}
	for _, r := range rows {
		if !slices.Contains(out, r.EventID) {
			out = append(out, r.EventID)
		}
	}
	return out
}

func decodeAnswers(j JSON) map[string]any {
	out := map[string]any{}
	if len(j) > 0 {
		_ = json.Unmarshal(j, &out)
	}
	return out
}

func decodeSnapshot(j JSON) map[string]bool {
	out := map[string]bool{}
	if len(j) > 0 {
		_ = json.Unmarshal(j, &out)
	}
	return out
}

// keysOf returns the sorted keys of m (never its values).
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, strings.Compare)
	return out
}
