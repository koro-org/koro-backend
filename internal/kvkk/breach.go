package kvkk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// The breach register (02 §16.4; KVKK md. 12/5): the 72-hour deadline of
// the notification to the Board and the 24-hour notice to the data
// controller (Proje Sahibi), both watched by kvkk.breach_timer.

const (
	// TopicBreachRecorded alerts the DPO and the platform admins (01 §6.2).
	TopicBreachRecorded = domainevent.TopicBreachRecorded
	// AlertBreachTaskName handles breach.recorded.
	AlertBreachTaskName = "kvkk.alert_breach"
	// BreachTimerJobName warns about the open deadlines (hourly :00,
	// critical; 01 §7).
	BreachTimerJobName = "kvkk.breach_timer"

	breachTimerCron   = "0 * * * *"
	breachTimerUnique = 55 * time.Minute

	tmplBreachRecorded = "kvkk.breach_recorded"
	tmplBreachDeadline = "kvkk.breach_deadline"

	maxBreachTitle       = 200
	maxBreachDescription = 10000
	maxCategories        = 30
	resourceBreach       = "breach_incident"

	// Steps of the breach timer.
	StepAuthority  = "authority"  // notification to the Board (72 hours)
	StepController = "controller" // notice to the data controller (24 hours)
)

// AuthorityWarnings are the hours left at which the Board deadline is
// warned about (01 §7: 48/24/6), 0 = overdue.
var AuthorityWarnings = []time.Duration{48 * time.Hour, 24 * time.Hour, 6 * time.Hour, 0}

// ControllerWarnings are the hours left of the 24-hour controller notice
// at which the DPO is warned, 0 = overdue.
var ControllerWarnings = []time.Duration{12 * time.Hour, 6 * time.Hour, 0}

var categoryRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,39}$`)

// BreachInput is the body of adminCreateBreach (04 §2.22).
type BreachInput struct {
	Title            string
	DetectedAt       time.Time
	DataCategories   []string
	AffectedEstimate *int
	Description      string
	OwnerUserID      uuid.UUID
}

// BreachPatch is the body of adminUpdateBreach; nil fields stay.
type BreachPatch struct {
	Title                *string
	Description          *string
	DataCategories       *[]string
	AffectedEstimate     *int
	OwnerUserID          *uuid.UUID
	DetectedAt           *time.Time // only earlier: the deadline moves with it
	Status               *BreachStatus
	ControllerNotifiedAt *time.Time // set once
	AuthorityNotifiedAt  *time.Time // set once
}

// categories validates the data category tokens (no free text: they are
// shown in lists and e-mails).
func categories(in []string, location string, required bool) ([]string, error) {
	if len(in) > maxCategories {
		return nil, invalid(location, "too many categories")
	}
	out := make([]string, 0, len(in))
	for _, c := range in {
		if !categoryRe.MatchString(c) {
			return nil, invalid(location, "categories are snake_case tokens (identity, contact, health, ...)")
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	if required && len(out) == 0 {
		return nil, invalid(location, "at least one category")
	}
	return out, nil
}

func breachEvent(a authz.Actor, b Breach, meta map[string]any) audit.Event {
	id := b.ID
	m := map[string]any{"status": string(b.Status)}
	for k, v := range meta {
		m[k] = v
	}
	return audit.Event{Action: string(authz.KvkkBreachManage), Outcome: audit.Success, ResourceType: resourceBreach,
		ResourceID: &id, Metadata: m}.By(a)
}

// userExists checks a user reference of a record.
func (s *service) userExists(ctx context.Context, id uuid.UUID) error {
	ok, err := s.d.Users.Exists(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrUnknownUser
	}
	return nil
}

// BreachPage is a page of adminListBreaches; Next is the position after
// its last row (nil: the last page).
type BreachPage struct {
	Items []Breach
	Next  *After
}

// ListBreaches implements Service (adminListBreaches): authz (403) → the
// status (422) → a page, the most recently detected first (BE-129: keyset
// on detected_at, id).
func (s *service) ListBreaches(ctx context.Context, a authz.Actor, status *BreachStatus, after *After, limit int) (BreachPage, error) {
	if err := authz.Can(ctx, a, authz.KvkkBreachList, adminSurface); err != nil {
		return BreachPage{}, err
	}
	if status != nil && !status.Valid() {
		return BreachPage{}, invalid("query.status", "unknown status")
	}
	limit = page.ClampLimit(limit)
	rows, err := s.d.Records.Breaches(ctx, status, after, limit+1)
	if err != nil {
		return BreachPage{}, err
	}
	items, more := page.Cut(rows, limit)
	p := BreachPage{Items: items}
	if more {
		last := items[len(items)-1]
		p.Next = &After{At: last.DetectedAt, ID: last.ID}
	}
	return p, nil
}

// CreateBreach implements Service (adminCreateBreach): the deadline of the
// Board notification is detected_at + 72 hours; breach.recorded alerts the
// DPO and the platform admins in the same transaction.
func (s *service) CreateBreach(ctx context.Context, a authz.Actor, in BreachInput) (Breach, error) {
	if err := authz.Can(ctx, a, authz.KvkkBreachManage, adminSurface); err != nil {
		return Breach{}, err
	}
	title, err := cleanText(in.Title, "body.title", maxBreachTitle, true)
	if err != nil {
		return Breach{}, err
	}
	desc, err := cleanText(in.Description, "body.description", maxBreachDescription, true)
	if err != nil {
		return Breach{}, err
	}
	cats, err := categories(in.DataCategories, "body.data_categories", false)
	if err != nil {
		return Breach{}, err
	}
	now := s.now()
	detected := in.DetectedAt.UTC().Truncate(time.Microsecond)
	switch {
	case in.DetectedAt.IsZero():
		return Breach{}, invalid("body.detected_at", "is required")
	case detected.After(now):
		return Breach{}, invalid("body.detected_at", "must not be in the future")
	case in.AffectedEstimate != nil && *in.AffectedEstimate < 0:
		return Breach{}, invalid("body.affected_estimate", "must not be negative")
	}
	if err := s.userExists(ctx, in.OwnerUserID); err != nil {
		return Breach{}, err
	}
	b := Breach{ID: uuid.NewV7(), Title: title, DetectedAt: detected, AuthorityDeadlineAt: detected.Add(AuthorityPeriod),
		DataCategories: cats, AffectedEstimate: in.AffectedEstimate, Description: desc, Status: BreachOpen,
		OwnerUserID: in.OwnerUserID}
	hash, err := idempotency.Hash(struct {
		In  BreachInput
		Cat []string
	}{in, cats})
	if err != nil {
		return Breach{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Breach, error) {
		if err := s.d.Records.InsertBreach(ctx, b); err != nil {
			return Breach{}, err
		}
		if err := s.d.Audit.Log(ctx, breachEvent(a, b, map[string]any{"step": "recorded",
			"authority_deadline_at": b.AuthorityDeadlineAt.Format(time.RFC3339)})); err != nil {
			return Breach{}, err
		}
		ev, err := domainevent.New(domainevent.BreachRecorded{IncidentID: b.ID, DeadlineAt: b.AuthorityDeadlineAt}, "breach", b.ID)
		if err != nil {
			return Breach{}, err
		}
		if err := s.d.Outbox.Add(ctx, ev); err != nil {
			return Breach{}, err
		}
		return s.d.Records.Breach(ctx, b.ID)
	})
	if err != nil {
		return Breach{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelWarn, "personal data breach recorded", slog.String("breach_id", out.ID.String()),
		slog.Time("authority_deadline_at", out.AuthorityDeadlineAt), slog.Bool("alarm", true))
	return out, nil
}

// notifiedAt validates a notification time: between detection and now.
func (s *service) notifiedAt(t time.Time, detected time.Time, location string) (time.Time, error) {
	at := t.UTC().Truncate(time.Microsecond)
	if at.After(s.now()) || at.Before(detected) {
		return time.Time{}, invalid(location, "must lie between detected_at and now")
	}
	return at, nil
}

// UpdateBreach implements Service (adminUpdateBreach, If-Match). The status
// only moves forward (open → contained → notified → closed); notified
// needs the Board notification time; a closed breach is final. detected_at
// may only move earlier (the deadline follows), so the clock is never
// reset; the notification times are written once.
func (s *service) UpdateBreach(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p BreachPatch) (Breach, error) {
	if err := authz.Can(ctx, a, authz.KvkkBreachManage, adminSurface); err != nil {
		return Breach{}, err
	}
	if p == (BreachPatch{}) {
		return Breach{}, invalid("body", "nothing to update")
	}
	if ifMatch == "" {
		return Breach{}, problem.ErrPreconditionRequired
	}
	var title, desc string
	var err error
	if p.Title != nil {
		if title, err = cleanText(*p.Title, "body.title", maxBreachTitle, true); err != nil {
			return Breach{}, err
		}
	}
	if p.Description != nil {
		if desc, err = cleanText(*p.Description, "body.description", maxBreachDescription, true); err != nil {
			return Breach{}, err
		}
	}
	var cats []string
	if p.DataCategories != nil {
		if cats, err = categories(*p.DataCategories, "body.data_categories", false); err != nil {
			return Breach{}, err
		}
	}
	if p.AffectedEstimate != nil && *p.AffectedEstimate < 0 {
		return Breach{}, invalid("body.affected_estimate", "must not be negative")
	}
	if p.Status != nil && !p.Status.Valid() {
		return Breach{}, invalid("body.status", "unknown status")
	}
	if p.OwnerUserID != nil {
		if err := s.userExists(ctx, *p.OwnerUserID); err != nil {
			return Breach{}, err
		}
	}
	var out Breach
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		b, err := s.d.Records.LockBreach(ctx, id)
		if err != nil {
			return err
		}
		if !ifMatches(ifMatch, b.UpdatedAt) {
			return problem.ErrPreconditionFailed
		}
		if b.Status == BreachClosed {
			return ErrState
		}
		var changed []string
		set := func(name string) { changed = append(changed, name) }
		if p.Title != nil {
			b.Title = title
			set("title")
		}
		if p.Description != nil {
			b.Description = desc
			set("description")
		}
		if p.DataCategories != nil {
			b.DataCategories = cats
			set("data_categories")
		}
		if p.AffectedEstimate != nil {
			v := *p.AffectedEstimate
			b.AffectedEstimate = &v
			set("affected_estimate")
		}
		if p.OwnerUserID != nil {
			b.OwnerUserID = *p.OwnerUserID
			set("owner_user_id")
		}
		if p.DetectedAt != nil {
			at := p.DetectedAt.UTC().Truncate(time.Microsecond)
			if at.After(b.DetectedAt) {
				return invalid("body.detected_at", "may only move earlier (the deadline is never extended)")
			}
			b.DetectedAt, b.AuthorityDeadlineAt = at, at.Add(AuthorityPeriod)
			set("detected_at")
		}
		for _, n := range []struct {
			in   *time.Time
			cur  **time.Time
			name string
		}{{p.ControllerNotifiedAt, &b.ControllerNotifiedAt, "controller_notified_at"},
			{p.AuthorityNotifiedAt, &b.AuthorityNotifiedAt, "authority_notified_at"}} {
			if n.in == nil {
				continue
			}
			at, err := s.notifiedAt(*n.in, b.DetectedAt, "body."+n.name)
			if err != nil {
				return err
			}
			if *n.cur != nil && !(*n.cur).Equal(at) {
				return ErrState // written once
			}
			*n.cur = &at
			set(n.name)
		}
		if p.Status != nil && *p.Status != b.Status {
			if p.Status.rank() < b.Status.rank() {
				return ErrState
			}
			b.Status = *p.Status
			set("status")
		}
		if b.Status == BreachNotified && b.AuthorityNotifiedAt == nil {
			return invalid("body.authority_notified_at", "status notified needs the time of the Board notification")
		}
		if b.Status == BreachClosed && b.ClosedAt == nil {
			now := s.now()
			b.ClosedAt = &now
		}
		if err := s.d.Records.UpdateBreach(ctx, b); err != nil {
			return err
		}
		if err := s.d.Audit.Log(ctx, breachEvent(a, b, map[string]any{"changed": strings.Join(changed, ",")})); err != nil {
			return err
		}
		out, err = s.d.Records.Breach(ctx, id)
		return err
	})
	if err != nil {
		return Breach{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "breach updated", slog.String("breach_id", id.String()), slog.String("status", string(out.Status)))
	return out, nil
}

// ---------------------------------------------------------------- alerts

// alertTimeFormat is how deadlines appear in the DPO e-mails.
func alertTime(t time.Time) string { return t.In(clock.Istanbul()).Format("2006-01-02 15:04") }

// staff returns the in-app recipients of a breach alert: the platform
// admins and the breach owner, each once.
func (s *service) staff(ctx context.Context, b Breach) ([]uuid.UUID, error) {
	var out []uuid.UUID
	if s.d.Admins != nil {
		admins, err := s.d.Admins(ctx)
		if err != nil {
			return nil, fmt.Errorf("kvkk: platform admins: %w", err)
		}
		out = append(out, admins...)
	} else {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "no platform admin resolver wired: breach alert goes to the DPO and the owner only",
			slog.String("breach_id", b.ID.String()))
	}
	if !slices.Contains(out, b.OwnerUserID) {
		out = append(out, b.OwnerUserID)
	}
	return out, nil
}

// alert sends one breach alert: the DPO mailbox (with the deadlines) and an
// in-app notice to every staff recipient (opaque IDs only, 05 notify).
func (s *service) alert(ctx context.Context, b Breach, template, key string, data map[string]any) error {
	data["breach_id"] = b.ID.String()
	if s.d.DPOEmail == "" {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "no DPO mailbox configured (KVKK_DPO_EMAIL): breach alert not mailed",
			slog.String("breach_id", b.ID.String()), slog.String("template", template))
	} else if err := s.send(ctx, notify.Message{ToEmail: s.d.DPOEmail, Channel: notify.ChannelEmail,
		Category: notify.CategoryTransactional, Template: template, Locale: "tr", DedupeKey: key, Data: data}, uuid.Nil()); err != nil {
		return err
	}
	to, err := s.staff(ctx, b)
	if err != nil {
		return err
	}
	for _, u := range to {
		uid := u
		if err := s.send(ctx, notify.Message{UserID: &uid, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional,
			Template: template, Data: map[string]any{"breach_id": b.ID.String()}, LinkPath: notify.AdminBreachesLink(),
			DedupeKey: key + ":" + uid.String()}, uuid.Nil()); err != nil {
			return err
		}
	}
	return nil
}

// handleBreachRecorded is the breach.recorded subscription
// (kvkk.alert_breach).
func (s *service) handleBreachRecorded(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.BreachRecorded](e)
	if err != nil {
		return queue.Permanent(err)
	}
	b, err := s.d.Records.Breach(ctx, p.IncidentID)
	switch {
	case errors.Is(err, ErrNotFound):
		return queue.Permanent(fmt.Errorf("kvkk: recorded breach %s not found", p.IncidentID))
	case err != nil:
		return err
	}
	return s.alert(ctx, b, tmplBreachRecorded, "kvkk.breach_recorded:"+b.ID.String(), map[string]any{
		"detected_at": alertTime(b.DetectedAt), "authority_deadline": alertTime(b.AuthorityDeadlineAt),
		"controller_deadline": alertTime(b.ControllerDeadlineAt())})
}

func (s *service) breachTimerJob() queue.Job {
	return queue.Job{Name: BreachTimerJobName, Cron: breachTimerCron, Queue: queue.Critical, Unique: breachTimerUnique,
		Handle: func(ctx context.Context) error { _, err := s.breachTimer(ctx); return err }}
}

// warningMark returns the tightest mark that left has reached (marks are
// descending; the last one is 0 = overdue); ok=false before the first.
func warningMark(left time.Duration, marks []time.Duration) (time.Duration, bool) {
	var mark time.Duration
	ok := false
	for _, m := range marks {
		if left <= m {
			mark, ok = m, true
		}
	}
	return mark, ok
}

// breachTimer is kvkk.breach_timer: for every breach that is not closed,
// the Board deadline (until authority_notified_at is recorded) at
// 48/24/6 hours left and when overdue, and the controller notice (until
// controller_notified_at) at 12/6 hours left and when overdue. Each
// (breach, step, mark) is sent once: the dedupe key carries the mark, so
// an hourly run repeats nothing and a missed run sends only the tightest
// mark reached. It returns the number of warnings sent.
func (s *service) breachTimer(ctx context.Context) (int, error) {
	now := s.now()
	bs, err := s.d.Records.Breaches(ctx, nil, nil, 0)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, b := range bs {
		if b.Status == BreachClosed {
			continue
		}
		for _, st := range []struct {
			step     string
			done     bool
			deadline time.Time
			marks    []time.Duration
		}{
			{StepController, b.ControllerNotifiedAt != nil, b.ControllerDeadlineAt(), ControllerWarnings},
			{StepAuthority, b.AuthorityNotifiedAt != nil, b.AuthorityDeadlineAt, AuthorityWarnings},
		} {
			if st.done {
				continue
			}
			left := st.deadline.Sub(now)
			mark, ok := warningMark(left, st.marks)
			if !ok {
				continue
			}
			hours := int(mark / time.Hour)
			key := "kvkk.breach_deadline:" + b.ID.String() + ":" + st.step + ":" + strconv.Itoa(hours)
			if err := s.alert(ctx, b, tmplBreachDeadline, key, map[string]any{"step": st.step, "hours_left": hours,
				"overdue": mark == 0, "deadline": alertTime(st.deadline)}); err != nil {
				return sent, err
			}
			sent++
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "breach deadline warning", slog.String("breach_id", b.ID.String()),
				slog.String("step", st.step), slog.Int("mark_hours", hours), slog.Bool("overdue", mark == 0),
				slog.Bool("alarm", mark == 0))
		}
	}
	return sent, nil
}
