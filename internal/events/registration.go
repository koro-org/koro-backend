package events

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
)

// Registrations (BE-028; 01 §6.3 "Etkinlik kaydı", 02 §11.6–11.7, 04
// §E.2, §E.6, R-003 §2.5.3). The chain of a registration runs in one
// transaction together with its Idempotency-Key (06 H-5):
//
//	conditional seat UPDATE → event_registrations → registration_tickets
//	(confirmed) → consent.Recorder (consent fields answered yes) → audit →
//	outbox (registration.confirmed | waitlisted | submitted)
//
// Lock order (no deadlock between the flows): the events row first (the
// seat UPDATE, LockEvent or ShareLockEvent), then registration rows.

// Audit resource type and outbox aggregate of registrations.
const regAggType = "registration"

// System jobs (authz.SystemActor names; 03 §3.7 actions).
const (
	promoteJob     = "events.promote_waitlist"
	expireJob      = "events.waitlist_expire"
	cancelEventJob = "events.cancel_registrations"
)

// Limits of the registration texts (02 §11.6 CHECKs).
const (
	maxDecisionNote  = 500
	maxRegCancel     = 300
	minOrgCancel     = 3
	maxPromotions    = 100 // seats filled from the waiting list in one transaction
	defaultAnswerTTL = 90 * 24 * time.Hour
	defaultSensTTL   = 30 * 24 * time.Hour
)

// Retention policy codes of the answer purge dates (02 §24.2).
const (
	retentionAnswers   = "event_form_answers"
	retentionSensitive = "event_form_answers_sensitive"
)

// SensitiveAnswersPurposePrefix is the envelope purpose prefix of
// answers_sensitive_enc (crypto.Seal("event_answers_sensitive:<id>"); the
// registration id binds a ciphertext to its row). crypto.rewrap rebuilds
// it from the id column.
const SensitiveAnswersPurposePrefix = "event_answers_sensitive:"

// SensitiveAnswersPurpose is the envelope purpose of a registration's
// sensitive answers.
func SensitiveAnswersPurpose(registrationID uuid.UUID) string {
	return SensitiveAnswersPurposePrefix + registrationID.String()
}

// RegistrationInput is the body of createRegistration (04 §2.6, §E.2).
type RegistrationInput struct {
	// FormVersionID is the version the answers were given to; a version
	// that is no longer current is 409 events.form_version_stale. nil:
	// the current version.
	FormVersionID *uuid.UUID
	Answers       map[string]any
	// Consents prove the consent text shown for every consent field
	// answered yes (consent.Recorder checks it).
	Consents []ConsentProof
	// Source is the client (X-Koro-Client: web, the default, or mobile); it
	// is also the consent channel.
	Source Source
}

// ConsentProof is one consents[] item of createRegistration.
type ConsentProof struct {
	Purpose         consent.Purpose
	NoticeVersionID uuid.UUID
	TextSHA256      []byte
}

// Access is the break-glass justification of an org read (03 §6.3):
// X-Koro-Access-Reason and the optional X-Koro-Ticket-Ref.
type Access struct {
	Reason    string
	TicketRef string
}

// Decision is the body of decideEventRegistration: confirmed, waitlisted
// or rejected, with an optional note the student never sees.
type Decision struct {
	Status RegStatus
	Note   string
}

// RegistrationView is a registration as one caller may see it.
type RegistrationView struct {
	Registration Registration
	Event        Event
	// Answers are the stored answers (owner and the organiser's views);
	// nil when the view carries none.
	Answers map[string]any
	// AnswersSensitive are the opened warn_special answers: only for the
	// owner (getMyRegistration) and the organiser's managers
	// (getEventRegistration), never in a list or by break-glass.
	AnswersSensitive map[string]any
	ConsentSnapshot  map[string]bool
	// WaitlistPosition is the current place on the waiting list.
	WaitlistPosition *int
	// TicketAvailable: the registration holds an active ticket.
	TicketAvailable bool
	// User is the registrant (the organiser's views only).
	User *Registrant
	// ShowNote: the decision note is shown (the organiser's managers; not
	// the student, not break-glass).
	ShowNote bool
}

// Registrant is the registrant card of the organiser's views (04 §V.6):
// no e-mail, phone, birth date, other registrations or talent.
type Registrant struct {
	UserID         uuid.UUID
	FirstName      string
	LastName       string
	AvatarURL      string
	UniversityName string
	DepartmentName string
	ClassYear      *int16
}

// MyRegistrationQuery narrows listMyRegistrations.
type MyRegistrationQuery struct {
	Statuses []RegStatus
	Upcoming bool // events that have not ended
	Limit    int
	After    *After
}

// EventRegistrationQuery narrows listEventRegistrations.
type EventRegistrationQuery struct {
	Statuses []RegStatus
	Desc     bool // sort -created_at
	Limit    int
	After    *After
}

// RegistrationPage is a page of registrations; Next is the keyset position
// of the next page (nil: last page).
type RegistrationPage struct {
	Items []RegistrationView
	Next  *After
}

// regForm is the form a registration answers.
type regForm struct {
	version   *FormVersion
	schema    FormSchema
	sensitive []string // warn_special keys of the version (02 §11.6)
}

// CreateRegistration implements Service. Order: the event (404) → authz on
// the event (404 invisible; 403, and 403 students.verification_required
// for an account without a student verification, k-uv) → in one
// transaction with the Idempotency-Key: the registration window (409), the
// form version (409 stale), the answers (422), the consent proofs (422),
// one live registration per user (409), the seat (confirmed, waitlisted or
// 409 events.capacity_full; manual mode: pending), then the chain of 01
// §6.3.
func (s *service) CreateRegistration(ctx context.Context, a authz.Actor, eventID uuid.UUID, in RegistrationInput) (RegistrationView, error) {
	e, err := s.d.Store.GetEvent(ctx, eventID)
	if err != nil {
		return RegistrationView{}, err
	}
	sens, err := s.sensitive(ctx, e)
	if err != nil {
		return RegistrationView{}, err
	}
	role, err := s.staffRole(ctx, a, e)
	if err != nil {
		return RegistrationView{}, err
	}
	if err := authz.Can(ctx, a, authz.EventsRegistrationCreate, eventResource(e, sens, role)); err != nil {
		if errors.Is(err, authz.ErrForbidden) && a.IsUser() && !a.IsVerifiedStudent() {
			return RegistrationView{}, orgs.ErrVerificationRequired // k-uv (T-54)
		}
		return RegistrationView{}, err
	}
	if in.Source == "" {
		in.Source = SourceWeb
	}
	if in.Source != SourceWeb && in.Source != SourceMobile {
		return RegistrationView{}, problem.Field("header.X-Koro-Client", "must be web or mobile")
	}
	hash, err := idempotency.Hash(struct {
		Op    string
		Event uuid.UUID
		In    RegistrationInput
	}{"register", e.ID, in})
	if err != nil {
		return RegistrationView{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		return s.register(ctx, a, e, sens, in)
	})
	if err != nil {
		return RegistrationView{}, err
	}
	return s.ownView(ctx, a.UserID, id, false)
}

// register is the transaction of CreateRegistration.
func (s *service) register(ctx context.Context, a authz.Actor, e Event, sens bool, in RegistrationInput) (uuid.UUID, error) {
	now := s.now()
	if err := registrationOpen(e, now); err != nil {
		return uuid.Nil(), err
	}
	form, err := s.registrationForm(ctx, e, in.FormVersionID)
	if err != nil {
		return uuid.Nil(), err
	}
	// A minor answers the reduced schema: no minor-restricted consent field
	// (T-22, 03 k12; BE-026 ForReader).
	schema := form.schema.ForReader(a.IsMinor)
	answers := in.Answers
	if answers == nil {
		answers = map[string]any{}
	}
	clean, err := ValidateAnswers(schema, answers)
	if err != nil {
		return uuid.Nil(), err
	}
	split := splitAnswers(schema, clean, form.sensitive)
	grants, err := consentGrants(schema, split.granted, in.Consents)
	if err != nil {
		return uuid.Nil(), err
	}
	if len(grants) > 0 && s.d.Consents == nil {
		return uuid.Nil(), problem.ErrUnavailable
	}
	if _, found, err := s.d.Store.ActiveRegistration(ctx, e.OrgID, e.ID, a.UserID); err != nil {
		return uuid.Nil(), err
	} else if found {
		return uuid.Nil(), ErrAlreadyRegistered
	}
	reg := Registration{
		ID: uuid.NewV7(), EventID: e.ID, OrgID: e.OrgID, UserID: a.UserID, Source: in.Source,
		Answers: mustJSON(split.plain), ConsentSnapshot: mustJSON(split.snapshot),
	}
	if form.version != nil {
		reg.FormVersionID = &form.version.ID
	}
	purge, sensPurge, err := s.purgeDates(ctx, e.EndsAt)
	if err != nil {
		return uuid.Nil(), err
	}
	reg.AnswersPurgeAt = purge
	if len(split.sensitive) > 0 {
		if s.d.Sealer == nil {
			return uuid.Nil(), problem.ErrUnavailable
		}
		enc, err := s.d.Sealer.Seal(ctx, SensitiveAnswersPurpose(reg.ID), mustJSON(split.sensitive))
		if err != nil {
			return uuid.Nil(), err
		}
		reg.AnswersSensitiveEnc, reg.SensitivePurgeAt = enc, &sensPurge
	}
	if reg.Status, err = s.admit(ctx, e, now); err != nil {
		return uuid.Nil(), err
	}
	switch reg.Status {
	case RegConfirmed:
		reg.ConfirmedAt = &now
	case RegWaitlisted:
		reg.WaitlistedAt = &now
	}
	stored, err := s.d.Store.InsertRegistration(ctx, reg)
	if err != nil {
		return uuid.Nil(), err
	}
	ticketVersion := 0
	if stored.Status == RegConfirmed {
		t, err := s.issueTicket(ctx, stored, now)
		if err != nil {
			return uuid.Nil(), err
		}
		ticketVersion = t.Version
	}
	for _, g := range grants {
		notice, sum := g.NoticeVersionID, g.TextSHA256
		if _, err := s.d.Consents.Record(ctx, consent.RecordInput{
			UserID: a.UserID, Purpose: g.Purpose, Action: consent.Granted, NoticeVersionID: &notice, TextSHA256: sum,
			ScopeID: &stored.ID, Channel: consentChannel(in.Source), Minor: a.IsMinor,
		}); err != nil {
			return uuid.Nil(), err
		}
	}
	meta := map[string]any{"event_id": e.ID.String(), "status": string(stored.Status), "consents": len(grants)}
	if stored.FormVersionID != nil {
		meta["form_version_id"] = stored.FormVersionID.String()
	}
	if err := s.d.Audit.Log(ctx, auditEvent(a, authz.EventsRegistrationCreate, regAggType, stored.ID, e.OrgID, "", meta)); err != nil {
		return uuid.Nil(), err
	}
	return stored.ID, s.emitAdmitted(ctx, stored, sens, false, ticketVersion)
}

// registrationOpen: a published event that takes registrations, inside its
// registration window and not over (04 §E.2).
func registrationOpen(e Event, now time.Time) error {
	switch {
	case e.Status == StatusDraft, !e.RequiresRegistration:
		return ErrInvalidState
	case e.Status != StatusPublished,
		e.RegistrationOpensAt != nil && now.Before(*e.RegistrationOpensAt),
		e.RegistrationClosesAt != nil && !now.Before(*e.RegistrationClosesAt),
		!now.Before(e.EndsAt):
		return ErrRegistrationClosed
	}
	return nil
}

// registrationForm returns the current published form of e; a requested
// version that is not the current one is 409 events.form_version_stale.
func (s *service) registrationForm(ctx context.Context, e Event, requested *uuid.UUID) (regForm, error) {
	empty := regForm{schema: FormSchema{SchemaVersion: FormSchemaVersion, Fields: []FormField{}}}
	f, err := s.d.Store.GetForm(ctx, e.OrgID, e.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		f = Form{}
	case err != nil:
		return regForm{}, err
	}
	if f.CurrentVersionID == nil {
		if requested != nil {
			return regForm{}, ErrFormVersionStale
		}
		return empty, nil
	}
	if requested != nil && *requested != *f.CurrentVersionID {
		return regForm{}, ErrFormVersionStale
	}
	v, err := s.d.Store.GetFormVersion(ctx, e.OrgID, *f.CurrentVersionID)
	if err != nil {
		return regForm{}, err
	}
	schema, err := DecodeFormSchema(v.Schema)
	if err != nil {
		return regForm{}, err
	}
	rep, err := DecodeLintReport(v.LintReport)
	if err != nil {
		return regForm{}, err
	}
	return regForm{version: &v, schema: schema, sensitive: rep.SensitiveKeys()}, nil
}

// splitResult is the stored form of the cleaned answers.
type splitResult struct {
	plain     map[string]any  // answers
	sensitive map[string]any  // answers_sensitive_enc (warn_special keys)
	snapshot  map[string]bool // consent_snapshot: purpose → answer
	granted   map[consent.Purpose]bool
}

// splitAnswers separates the answers: consent fields go to the snapshot
// (the consent ledger is the record of truth), warn_special fields to the
// encrypted column, the rest to answers (02 §11.6).
func splitAnswers(schema FormSchema, clean map[string]any, sensitive []string) splitResult {
	out := splitResult{plain: map[string]any{}, sensitive: map[string]any{}, snapshot: map[string]bool{},
		granted: map[consent.Purpose]bool{}}
	for _, f := range schema.Fields {
		v, ok := clean[f.Key]
		if !ok {
			continue
		}
		switch {
		case f.Type == FieldConsent:
			yes, _ := v.(bool)
			out.snapshot[f.ConsentPurpose] = yes
			if yes {
				out.granted[consent.Purpose(f.ConsentPurpose)] = true
			}
		case slices.Contains(sensitive, f.Key):
			out.sensitive[f.Key] = v
		default:
			out.plain[f.Key] = v
		}
	}
	return out
}

// consentGrants pairs every consent field answered yes with its proof in
// consents[]; a missing proof, or a proof without such a field, is 422.
func consentGrants(schema FormSchema, granted map[consent.Purpose]bool, proofs []ConsentProof) ([]ConsentProof, error) {
	var errs []problem.FieldError
	used := make([]bool, len(proofs))
	out := []ConsentProof{}
	for _, f := range schema.Fields {
		p := consent.Purpose(f.ConsentPurpose)
		if f.Type != FieldConsent || !granted[p] {
			continue
		}
		i := slices.IndexFunc(proofs, func(c ConsentProof) bool { return c.Purpose == p })
		if i < 0 {
			errs = append(errs, problem.FieldError{Location: "body.consents",
				Message: "must prove the consent text of " + string(p) + " (answers." + f.Key + " is true)"})
			continue
		}
		used[i] = true
		out = append(out, proofs[i])
	}
	for i, c := range proofs {
		loc := "body.consents[" + strconv.Itoa(i) + "]"
		switch {
		case !used[i]:
			errs = append(errs, problem.FieldError{Location: loc, Message: "does not match a consent field answered true"})
		case len(c.TextSHA256) != 32:
			errs = append(errs, problem.FieldError{Location: loc + ".text_sha256", Message: "must be a SHA-256 digest"})
		}
	}
	if len(errs) > 0 {
		return nil, problem.ErrValidation.WithFields(errs...)
	}
	return out, nil
}

// admit takes the registration's place: manual mode → pending (under a
// share lock, so a concurrent cancellation of the event waits for us);
// auto mode → a seat by the conditional UPDATE, else, under the event's
// row lock, one more try (a seat freed meanwhile) or the waiting list
// (open and the event not started) or 409 events.capacity_full.
func (s *service) admit(ctx context.Context, e Event, now time.Time) (RegStatus, error) {
	if e.RegistrationMode == ModeManual {
		cur, err := s.d.Store.ShareLockEvent(ctx, e.OrgID, e.ID)
		if err != nil {
			return "", err
		}
		if cur.Status != StatusPublished {
			return "", ErrRegistrationClosed
		}
		return RegPending, nil
	}
	if ok, err := s.d.Store.ClaimSeat(ctx, e.OrgID, e.ID); err != nil {
		return "", err
	} else if ok {
		return RegConfirmed, nil
	}
	cur, err := s.d.Store.LockEvent(ctx, e.ID)
	switch {
	case err != nil:
		return "", err
	case cur.OrgID != e.OrgID:
		return "", ErrNotFound
	case cur.Status != StatusPublished:
		return "", ErrRegistrationClosed
	}
	if ok, err := s.d.Store.ClaimSeat(ctx, e.OrgID, e.ID); err != nil {
		return "", err
	} else if ok {
		return RegConfirmed, nil
	}
	if cur.WaitlistEnabled && now.Before(cur.StartsAt) {
		return RegWaitlisted, nil
	}
	return "", ErrCapacityFull
}

// emitAdmitted writes the event of a new or decided registration (O):
// confirmed (with its ticket version), waitlisted or, for a manual
// application, submitted.
func (s *service) emitAdmitted(ctx context.Context, r Registration, sens, promoted bool, ticketVersion int) error {
	var p domainevent.Payload
	switch r.Status {
	case RegConfirmed:
		p = domainevent.RegistrationConfirmed{RegistrationID: r.ID, EventID: r.EventID, OrgID: r.OrgID, UserID: r.UserID,
			Sensitive: sens, Promoted: promoted, TicketVersion: ticketVersion}
	case RegWaitlisted:
		p = domainevent.RegistrationWaitlisted{RegistrationID: r.ID, EventID: r.EventID, UserID: r.UserID}
	case RegPending:
		p = domainevent.RegistrationSubmitted{RegistrationID: r.ID, EventID: r.EventID, OrgID: r.OrgID, UserID: r.UserID}
	case RegRejected:
		p = domainevent.RegistrationRejected{RegistrationID: r.ID, EventID: r.EventID, UserID: r.UserID}
	default:
		return nil
	}
	return s.emitReg(ctx, p, r.ID)
}

// emitReg writes a registration event to the outbox in the transaction.
func (s *service) emitReg(ctx context.Context, p domainevent.Payload, registrationID uuid.UUID) error {
	return outbox.Emit(ctx, s.d.Outbox, p, regAggType, registrationID)
}

// purgeDates are answers_purge_at and sensitive_purge_at of an event ending
// at end (02 §11.6; retention_policies event_form_answers and
// event_form_answers_sensitive, defaults 90 and 30 days).
func (s *service) purgeDates(ctx context.Context, end time.Time) (time.Time, time.Time, error) {
	answers, err := s.retention(ctx, retentionAnswers, defaultAnswerTTL)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	sens, err := s.retention(ctx, retentionSensitive, defaultSensTTL)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return end.Add(answers), end.Add(sens), nil
}

func (s *service) retention(ctx context.Context, code string, def time.Duration) (time.Duration, error) {
	if s.d.Retention == nil {
		return def, nil
	}
	d, found, err := s.d.Retention(ctx, code)
	switch {
	case err != nil:
		return 0, err
	case !found || d <= 0:
		return def, nil
	}
	return d, nil
}

// consentChannel is the consent channel of a registration source.
func consentChannel(src Source) consent.Channel {
	if src == SourceMobile {
		return consent.ChannelMobile
	}
	return consent.ChannelWeb
}

// mustJSON encodes answers (decoded JSON values: always encodable).
func mustJSON(v any) JSON {
	b, err := json.Marshal(v)
	if err != nil {
		panic("events: answers do not encode: " + err.Error())
	}
	return JSON(b)
}
