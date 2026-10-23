package talent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/students"
)

// Subscriptions of talent (01 §6.2, §8 "<module>.<verb>_<object>"). The
// signal consumers run on the low queue (01 §8: talent); the two
// neutralisation consumers on default (a KVKK objection and an opt-out
// must not wait behind a backlog of signals).
const (
	IngestRegistrationTaskName   = "talent.ingest_registration"    // registration.confirmed
	ReverseRegistrationTaskName  = "talent.reverse_registration"   // registration.cancelled
	IngestNoShowTaskName         = "talent.ingest_no_show"         // registration.no_show
	IngestCheckinTaskName        = "talent.ingest_checkin"         // checkin.recorded
	ReverseCheckinTaskName       = "talent.reverse_checkin"        // checkin.voided
	IngestMembershipTaskName     = "talent.ingest_membership"      // membership.started
	ReverseMembershipTaskName    = "talent.reverse_membership"     // membership.ended
	IngestProfileSectionTaskName = "talent.ingest_profile_section" // student.profile_section_completed
	ApplyRankingChoiceTaskName   = "talent.apply_ranking_choice"   // student.ranking_choice_changed
	IngestLikeTaskName           = "talent.ingest_like"            // post.liked
	ReverseLikeTaskName          = "talent.reverse_like"           // post.unliked
	IngestSaveTaskName           = "talent.ingest_save"            // post.saved
	IngestActiveDayTaskName      = "talent.ingest_active_day"      // user.active_day
	NeutralizeObjectionTaskName  = "talent.neutralize_objection"   // dsr.profiling_objection
)

// Subscribe registers the consumers of 01 §4.11 / §6.2.
func (m Module) Subscribe(r *outbox.Registry) {
	low, def := outbox.Queue(queue.Low), outbox.Queue(queue.Default)
	s := m.svc
	r.Subscribe(domainevent.TopicRegistrationConfirmed, IngestRegistrationTaskName, s.handleRegistrationConfirmed, low)
	r.Subscribe(domainevent.TopicRegistrationCancelled, ReverseRegistrationTaskName, s.handleRegistrationCancelled, low)
	r.Subscribe(domainevent.TopicRegistrationNoShow, IngestNoShowTaskName, s.handleNoShow, low)
	r.Subscribe(domainevent.TopicCheckinRecorded, IngestCheckinTaskName, s.handleCheckinRecorded, low)
	r.Subscribe(domainevent.TopicCheckinVoided, ReverseCheckinTaskName, s.handleCheckinVoided, low)
	r.Subscribe(domainevent.TopicMembershipStarted, IngestMembershipTaskName, s.handleMembershipStarted, low)
	r.Subscribe(domainevent.TopicMembershipEnded, ReverseMembershipTaskName, s.handleMembershipEnded, low)
	r.Subscribe(domainevent.TopicStudentProfileSectionCompleted, IngestProfileSectionTaskName, s.handleProfileSection, low)
	r.Subscribe(domainevent.TopicStudentRankingChoiceChanged, ApplyRankingChoiceTaskName, s.handleRankingChoice, def)
	r.Subscribe(domainevent.TopicPostLiked, IngestLikeTaskName, s.handlePostLiked, low)
	r.Subscribe(domainevent.TopicPostUnliked, ReverseLikeTaskName, s.handlePostUnliked, low)
	r.Subscribe(domainevent.TopicPostSaved, IngestSaveTaskName, s.handlePostSaved, low)
	r.Subscribe(domainevent.TopicUserActiveDay, IngestActiveDayTaskName, s.handleActiveDay, low)
	r.Subscribe(domainevent.TopicDSRProfilingObjection, NeutralizeObjectionTaskName, s.handleObjection, def)
}

// ---------------------------------------------------------------- handlers

func (s *service) handleRegistrationConfirmed(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.RegistrationConfirmed](e)
	if err != nil {
		return queue.Permanent(err)
	}
	c := s.newCredit(e, IngestRegistrationTaskName, SignalRegistrationConfirmed, p.UserID, SourceRegistration, p.RegistrationID, p.Sensitive)
	c.event, c.organiser = p.EventID, p.OrgID
	return s.credit(ctx, c)
}

func (s *service) handleRegistrationCancelled(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.RegistrationCancelled](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.reverse(ctx, reversal{task: ReverseRegistrationTaskName, reason: "registration_cancelled", eventID: e.ID,
		key: SourceKey{UserID: p.UserID, Signal: SignalRegistrationConfirmed, SourceType: SourceRegistration, SourceID: p.RegistrationID}})
}

func (s *service) handleNoShow(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.RegistrationNoShow](e)
	if err != nil {
		return queue.Permanent(err)
	}
	c := s.newCredit(e, IngestNoShowTaskName, SignalEventNoShow, p.UserID, SourceRegistration, p.RegistrationID, p.Sensitive)
	c.event = p.EventID // the payload has no org: the organiser comes from Events
	return s.credit(ctx, c)
}

// handleCheckinRecorded: a self check-in (the attendee scanned their own
// ticket) and the check-in of an organiser of the event's org are recorded
// with 0 points (ADR-0015: the organiser earns nothing at their own event).
// A payload without OrgID (published before BE-085) takes the organiser
// from Events.
func (s *service) handleCheckinRecorded(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.CheckinRecorded](e)
	if err != nil {
		return queue.Permanent(err)
	}
	c := s.newCredit(e, IngestCheckinTaskName, SignalEventCheckin, p.UserID, SourceCheckin, p.CheckinID, p.Sensitive)
	c.event, c.organiser = p.EventID, p.OrgID
	c.zero = p.StaffUserID == p.UserID
	return s.credit(ctx, c)
}

func (s *service) handleCheckinVoided(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.CheckinVoided](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.reverse(ctx, reversal{task: ReverseCheckinTaskName, reason: "checkin_voided", eventID: e.ID,
		key: SourceKey{UserID: p.UserID, Signal: SignalEventCheckin, SourceType: SourceCheckin, SourceID: p.CheckinID}})
}

// handleMembershipStarted: club memberships only; a sensitive club never
// produces a signal (02 §15.3, 03 Y2); at most 2 clubs count.
func (s *service) handleMembershipStarted(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.MembershipStarted](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if authz.OrgKind(p.OrgKind) != authz.OrgClub {
		return nil
	}
	c := s.newCredit(e, IngestMembershipTaskName, SignalClubMembership, p.UserID, SourceMembership, p.MembershipID, false)
	c.clubs, c.maxActive = []uuid.UUID{p.OrgID}, maxActiveClubs
	return s.credit(ctx, c)
}

func (s *service) handleMembershipEnded(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.MembershipEnded](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if authz.OrgKind(p.OrgKind) != authz.OrgClub {
		return nil
	}
	return s.reverse(ctx, reversal{task: ReverseMembershipTaskName, reason: "membership_ended", eventID: e.ID,
		key: SourceKey{UserID: p.UserID, Signal: SignalClubMembership, SourceType: SourceMembership, SourceID: p.MembershipID}})
}

// handleProfileSection: once per section and user (the source is derived
// from both).
func (s *service) handleProfileSection(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.StudentProfileSectionCompleted](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.Section == "" {
		return queue.Permanent(errors.New("talent: profile section without a name"))
	}
	src := derivedID("profile_section", p.UserID.String(), p.Section)
	return s.credit(ctx, s.newCredit(e, IngestProfileSectionTaskName, SignalProfileSection, p.UserID, SourceProfileSection, src, false))
}

// handlePostLiked: once per post (an unlike reverses, a second like is a
// duplicate); a post of a sensitive club produces nothing.
func (s *service) handlePostLiked(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PostLiked](e)
	if err != nil {
		return queue.Permanent(err)
	}
	c := s.newCredit(e, IngestLikeTaskName, SignalFeedLike, p.UserID, SourcePost, p.PostID, false)
	c.clubs = []uuid.UUID{p.AuthorOrgID}
	return s.credit(ctx, c)
}

func (s *service) handlePostUnliked(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PostUnliked](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.reverse(ctx, reversal{task: ReverseLikeTaskName, reason: "post_unliked", eventID: e.ID,
		key: SourceKey{UserID: p.UserID, Signal: SignalFeedLike, SourceType: SourcePost, SourceID: p.PostID}})
}

func (s *service) handlePostSaved(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PostSaved](e)
	if err != nil {
		return queue.Permanent(err)
	}
	c := s.newCredit(e, IngestSaveTaskName, SignalFeedSave, p.UserID, SourcePost, p.PostID, false)
	c.clubs = []uuid.UUID{p.AuthorOrgID}
	return s.credit(ctx, c)
}

// handleActiveDay: once per user and calendar day (Europe/Istanbul; the
// payload's day is the counter day).
func (s *service) handleActiveDay(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.UserActiveDay](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.Day.IsZero() {
		return queue.Permanent(errors.New("talent: active day without a day"))
	}
	c := s.newCredit(e, IngestActiveDayTaskName, SignalActiveDay, p.UserID, SourceDay, derivedID("day", p.UserID.String(), p.Day.String()), false)
	c.day = p.Day
	return s.credit(ctx, c)
}

// ---------------------------------------------------------------- credit

// credit is one signal to write.
type credit struct {
	task      string // subscription name: the system job
	signal    string
	key       SourceKey
	at        time.Time  // occurred_at: the event's instant
	day       civil.Date // counter day (Europe/Istanbul)
	eventID   uuid.UUID  // Envelope.ID (source_event_id)
	sensitive bool       // the producer flagged the source sensitive
	clubs     []uuid.UUID
	zero      bool // recorded with 0 points (per_source): self check-in, organiser
	maxActive int  // at most this many credited rows of the signal (0: no limit)

	// event is the Koro event of an event signal and organiser its org from
	// the payload (uuid.Nil: unknown, read through Events); the organiser
	// rule sets zero (screen).
	event, organiser uuid.UUID
	// created is the account's creation instant of a like (screen; nil:
	// unknown account), checked against min_account_age_days_for_likes in
	// the transaction.
	created *time.Time
}

func (s *service) newCredit(e domainevent.Envelope, task, signal string, userID uuid.UUID, srcType string, srcID uuid.UUID, sensitive bool) credit {
	at := e.OccurredAt.UTC().Truncate(time.Microsecond)
	if e.OccurredAt.IsZero() {
		at = s.now()
	}
	return credit{task: task, signal: signal, at: at, day: civil.From(at.In(clock.Istanbul())), eventID: e.ID, sensitive: sensitive,
		key: SourceKey{UserID: userID, Signal: signal, SourceType: srcType, SourceID: srcID}}
}

// credit writes one signal: the flag, the sensitivity and the student
// checks run first on app_rw, the ledger row in a talent transaction.
// The outcome is counted after the commit (a retried task counts once more
// as a duplicate, never twice as written).
func (s *service) credit(ctx context.Context, c credit) error {
	if err := system(ctx, c.task); err != nil {
		return queue.Permanent(err)
	}
	if !s.d.CollectionEnabled {
		s.counts.inc(c.signal, OutcomeFlagOff)
		return nil
	}
	outcome, err := s.screen(ctx, &c)
	if err != nil {
		return err
	}
	if outcome == "" {
		err = s.inTx(ctx, func(ctx context.Context) error {
			var err error
			outcome, err = s.write(ctx, c)
			return err
		})
		if err != nil {
			return err
		}
	}
	s.counts.inc(c.signal, outcome)
	return nil
}

// screen returns the outcome that stops c before the ledger ("" = go on)
// and, for an eligible subject, completes c with the public reads the
// transaction cannot make: the organiser rule (zero) and a liker's account
// age.
func (s *service) screen(ctx context.Context, c *credit) (string, error) {
	if c.sensitive {
		return OutcomeSensitive, nil
	}
	if len(c.clubs) > 0 {
		sens, err := s.d.Directory.SensitiveOrgIDs(ctx, c.clubs)
		if err != nil {
			return "", fmt.Errorf("talent: sensitive clubs: %w", err)
		}
		for _, id := range c.clubs {
			if sens[id] {
				return OutcomeSensitive, nil
			}
		}
	}
	if outcome, err := s.studentOutcome(ctx, c.key.UserID); outcome != "" || err != nil {
		return outcome, err
	}
	if !c.zero && c.event != uuid.Nil() {
		org, err := s.organises(ctx, c.key.UserID, c.event, c.organiser)
		if err != nil {
			return "", err
		}
		c.zero = org
	}
	if c.signal == SignalFeedLike {
		created, err := s.d.Accounts.AccountAge(ctx, []uuid.UUID{c.key.UserID})
		if err != nil {
			return "", fmt.Errorf("talent: account age: %w", err)
		}
		if at, ok := created[c.key.UserID]; ok {
			c.created = &at
		}
	}
	return "", nil
}

// organises reports whether userID organises the event: an active
// membership of the organising org (org; uuid.Nil → the event's org from
// Events, unknown event → no) in a role other than a participant's
// (organiserRole).
func (s *service) organises(ctx context.Context, userID, event, org uuid.UUID) (bool, error) {
	if org == uuid.Nil() {
		facts, err := s.d.Events.EventFacts(ctx, []uuid.UUID{event})
		if err != nil {
			return false, fmt.Errorf("talent: event facts: %w", err)
		}
		f, ok := facts[event]
		if !ok {
			return false, nil
		}
		org = f.OrgID
	}
	ms, err := s.d.Memberships.MembershipsOf(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("talent: memberships: %w", err)
	}
	return slices.ContainsFunc(ms, func(m authz.Membership) bool { return m.OrgID == org && organiserRole(m.Role) }), nil
}

// organiserRole: every org role organises the org's events (club_admin,
// university_rep and career_staff manage them, 03 §5.6; the company and
// platform roles hold the org) except the participants': a club's
// ordinary member (and a phase 2 NGO member) is the audience.
func organiserRole(r authz.MemberRole) bool {
	return r != authz.MemberMember && r != authz.MemberNGOMember
}

// studentOutcome checks the subject: a student profile, an adult, ranking
// not turned off ("" = eligible).
func (s *service) studentOutcome(ctx context.Context, userID uuid.UUID) (string, error) {
	cohorts, err := s.d.Students.Cohorts(ctx, []uuid.UUID{userID})
	if err != nil {
		return "", fmt.Errorf("talent: cohort: %w", err)
	}
	co, ok := cohorts[userID]
	switch {
	case !ok:
		return OutcomeNotStudent, nil
	case co.IsMinor:
		return OutcomeMinor, nil
	}
	choice, err := s.d.Students.RankingChoice(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("talent: ranking choice: %w", err)
	}
	if choice == students.RankingDisabled {
		return OutcomeOptedOut, nil
	}
	return "", nil
}

// write appends the ledger row of c in the talent transaction of ctx and
// returns its outcome.
func (s *service) write(ctx context.Context, c credit) (string, error) {
	st := s.d.Store
	if stop, err := s.collectionStopped(ctx, c.key.UserID); err != nil || stop {
		return OutcomeNeutralized, err
	}
	switch _, err := st.FindPoint(ctx, c.key); {
	case err == nil:
		return OutcomeDuplicate, nil
	case !errors.Is(err, ErrNotFound):
		return "", err
	}
	def, err := st.SignalDef(ctx, c.signal)
	switch {
	case errors.Is(err, ErrNotFound):
		return OutcomeSignalDisabled, nil
	case err != nil:
		return "", err
	case !def.Enabled:
		return OutcomeSignalDisabled, nil
	}
	cfg, err := st.CurrentConfig(ctx, s.now())
	switch {
	case errors.Is(err, ErrNotFound):
		return OutcomeNoConfig, nil
	case err != nil:
		return "", err
	}
	if c.signal == SignalFeedLike {
		g, err := gamingOf(cfg.Params)
		if err != nil {
			return "", err
		}
		if days := minDays(g.MinAccountAgeDaysForLikes); days > 0 && !oldEnough(c.created, c.at, days) {
			return OutcomeYoungAccount, nil
		}
	}

	applied, capReason, counted, err := s.applyCaps(ctx, c, def)
	if err != nil {
		return "", err
	}
	points := applied
	if def.BasePointsCP < 0 {
		points = -applied
	}
	row := PointEvent{ID: uuid.NewV7(), UserID: c.key.UserID, Signal: c.signal, SourceType: c.key.SourceType, SourceID: c.key.SourceID,
		OccurredAt: c.at, RawPointsCP: def.BasePointsCP, PointsCP: points, Capped: capReason != "", CapReason: capReason,
		ReasonCode: c.signal, ConfigVersionID: cfg.ID, SourceEventID: c.eventID}
	if _, inserted, err := st.InsertPoint(ctx, row); err != nil || !inserted {
		return OutcomeDuplicate, err
	}
	if counted && applied > 0 {
		if err := st.AddCounter(ctx, c.key.UserID, c.day, c.signal, applied); err != nil {
			return "", err
		}
	}
	if capReason != "" {
		return OutcomeCapped, nil
	}
	return OutcomeWritten, nil
}

// applyCaps returns the magnitude to apply (caps are magnitudes, the sign
// comes from the definition), the binding cap and whether the daily
// counter row was locked (and must be advanced).
func (s *service) applyCaps(ctx context.Context, c credit, def SignalDef) (applied int, capReason string, counted bool, err error) {
	st := s.d.Store
	applied = abs(def.BasePointsCP)
	switch {
	case c.zero:
		return 0, CapPerSource, false, nil
	case c.maxActive > 0:
		n, err := st.CountCredited(ctx, c.key.UserID, c.signal)
		if err != nil {
			return 0, "", false, err
		}
		if n >= c.maxActive {
			return 0, CapPerSource, false, nil
		}
	}
	if applied == 0 || (def.DailyCapCP == nil && def.WeeklyCapCP == nil) {
		return applied, "", false, nil
	}
	today, err := st.LockCounter(ctx, c.key.UserID, c.day, c.signal)
	if err != nil {
		return 0, "", false, err
	}
	if d := def.DailyCapCP; d != nil {
		if rem := max(0, *d-today); rem < applied {
			applied, capReason = rem, CapDaily
		}
	}
	if w := def.WeeklyCapCP; w != nil {
		week, err := st.AppliedBetween(ctx, c.key.UserID, c.signal, c.day.AddDays(-6), c.day)
		if err != nil {
			return 0, "", false, err
		}
		if rem := max(0, *w-week); rem < applied {
			applied, capReason = rem, CapWeekly
		}
	}
	return applied, capReason, true, nil
}

// collectionStopped reports an active opt-out or objection of the user.
func (s *service) collectionStopped(ctx context.Context, userID uuid.UUID) (bool, error) {
	ns, err := s.d.Store.Neutralizations(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, n := range ns {
		if n.Active() && n.Reason.stopsCollection() {
			return true, nil
		}
	}
	return false, nil
}

// ---------------------------------------------------------------- reverse

// reversal cancels the ledger row of key.
type reversal struct {
	task    string
	reason  string // reason_code of the reversal row
	key     SourceKey
	eventID uuid.UUID
}

// reverse writes the reversal of r's original: points and raw points
// negated, the original's occurred_at (the decay matches) and config
// version. No original (the signal was never written: flag off,
// ineligible, sensitive) → nothing to do. With the flag off nothing is
// written at all (T-32), reversals included.
func (s *service) reverse(ctx context.Context, r reversal) error {
	if err := system(ctx, r.task); err != nil {
		return queue.Permanent(err)
	}
	if !s.d.CollectionEnabled {
		s.counts.inc(r.key.Signal, OutcomeFlagOff)
		return nil
	}
	var outcome string
	err := s.inTx(ctx, func(ctx context.Context) error {
		orig, err := s.d.Store.FindPoint(ctx, r.key)
		switch {
		case errors.Is(err, ErrNotFound):
			outcome = OutcomeNoOriginal
			return nil
		case err != nil:
			return err
		}
		id := orig.ID
		row := PointEvent{ID: uuid.NewV7(), UserID: orig.UserID, Signal: orig.Signal, SourceType: SourceReversal, SourceID: orig.ID,
			OccurredAt: orig.OccurredAt, RawPointsCP: -orig.RawPointsCP, PointsCP: -orig.PointsCP, ReasonCode: r.reason,
			ConfigVersionID: orig.ConfigVersionID, ReversalOf: &id, SourceEventID: r.eventID}
		_, inserted, err := s.d.Store.InsertPoint(ctx, row)
		outcome = OutcomeReversed
		if !inserted {
			outcome = OutcomeDuplicate
		}
		return err
	})
	if err != nil {
		return err
	}
	s.counts.inc(r.key.Signal, outcome)
	return nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
