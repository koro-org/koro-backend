package consent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/iys"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

// İYS status reconciliation (BE-103, BE-105; 00 T-312, T-354). A recipient
// may refuse commercial messages in İYS itself (İYS web, mobile, e-Devlet,
// 3338, the call centre); Koro's gates read the ledger alone
// (FilterActive), so such a refusal must reach the ledger. Under 6563 the
// state İYS holds is what a commercial message is checked against.
//
// consent.iys_status_sync runs hourly over the İYS readers of the adapter
// (IYSFeedReader, IYSStateReader) and appends a withdrawal (channel iys, the system actor
// consent.iys_status_sync, audited, never sent back to İYS) where İYS
// holds a refusal:
//
//  1. Change feed (IYSFeedReader; the primary read): the approvals
//     and refusals recipients gave İYS itself since the stored cursor
//     (iys_change_feeds, one row per feed), pulled from İYS's change list
//     or pushed by an integrator. A refusal dated at or after the grant in
//     force withdraws it ("refused"); an older one is superseded by the
//     newer grant once İYS has it (step 2 covers grants it does not). An
//     İYS approval never becomes a Koro grant.
//  2. Unconfirmed grants (IYSStateReader): a grant in force for
//     longer than IYSInFlightGrace whose İYS submission is still pending or
//     failed is read one by one; where İYS still holds an older refusal,
//     İYS did not take Koro's approval and its state governs sending →
//     withdrawn ("conflict", an alarm for the DPO).
//  3. Full reconciliation (IYSStateReader): when the feed lost
//     changes (its first read; a horizon older than the feed's window,
//     seven days for İYS's change list) every grant in force is read one
//     by one as in BE-103: İYS refusal at or after the grant → "refused";
//     older → nothing within IYSInFlightGrace, "conflict" after it; no
//     record → counted (missing). It is marked due in iys_change_feeds and
//     repeated each run until it completes. An adapter without a feed does
//     this on every run; an adapter without single reads cannot do it while
//     a grant is in force: the run stays incomplete (alarm) and the hold
//     keeps commercial messages.
//
// The run is idempotent: a withdrawn chain is no longer a grant, and every
// withdrawal re-reads the chain under the subject lock, so a grant or a
// withdrawal recorded meanwhile is left to the next run. A channel whose
// reads all completed writes its iys_status_syncs row: synced_at is the
// feed's horizon (every refusal İYS recorded before it is in the ledger;
// İYS's change list hides its last hour) or the start of the run without a
// feed. The optional hold (CONSENT_IYS_SYNC_MAX_AGE) reads it.

// ErrIYSSyncStale is FilterActive's answer for an İYS purpose while the
// send-time hold is on (CONSENT_IYS_SYNC_MAX_AGE) and the channel's last
// complete İYS reconciliation is older than that, or never ran. A refusal
// given in İYS meanwhile would not be in the ledger yet: callers keep the
// message for later (outreach's dispatch goes back to scheduled and is
// retried; BE-103).
var ErrIYSSyncStale = errors.New("consent: İYS consent states are not reconciled recently enough; commercial messages are held")

// errIYSSweepUnavailable: the feed lost changes and the adapter reads no
// single states, so the grants in force cannot be reconciled.
var errIYSSweepUnavailable = errors.New("consent: the İYS change feed lost changes and the İYS adapter cannot read single " +
	"states to reconcile the grants in force")

const (
	// StatusSyncJobName is consent.iys_status_sync (BE-103): hourly at :20.
	StatusSyncJobName = "consent.iys_status_sync"
	// IYSStatusBatch is the number of users per Statuses call (Koro's own
	// chunk; the adapter may split further).
	IYSStatusBatch = iys.MaxStatusBatch
	// IYSInFlightGrace is how long a grant newer than İYS's refusal is
	// assumed to be on its way to İYS (delivery, retries, İYS processing;
	// 6563 gives the brand 3 business days to register a consent).
	IYSInFlightGrace = 72 * time.Hour

	statusSyncCron    = "20 * * * *"
	statusSyncUnique  = 50 * time.Minute
	statusSyncTimeout = 30 * time.Minute
	// maxFeedPages bounds the feed pages of one run (İYS: 1000 per page);
	// the next run continues from the stored cursor.
	maxFeedPages = 100
	// dueSubmissionBatch pages the unconfirmed-grant check.
	dueSubmissionBatch = 1000

	// Reasons of an İYS withdrawal (audit iys_reason).
	iysReasonRefused  = "refused"
	iysReasonConflict = "conflict"
)

// reIYSSourceCode is a plain İYS source code.
var reIYSSourceCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)

// statusSyncJob is consent.iys_status_sync; scheduled only with a reader.
func (s *service) statusSyncJob() queue.Job {
	return queue.Job{
		Name: StatusSyncJobName, Cron: statusSyncCron, Queue: queue.Default, Unique: statusSyncUnique,
		Timeout: statusSyncTimeout, Handle: s.syncIYSStatus,
	}
}

// syncTally counts one channel of a run (logged; never personal data).
type syncTally struct {
	checked, withdrawn, conflicts, inFlight, missing, superseded int
}

// syncRun is one run of consent.iys_status_sync.
type syncRun struct {
	s        *service
	feed     IYSFeedReader  // nil: the adapter has no change feed
	states   IYSStateReader // nil: the adapter reads no single states
	started  time.Time
	channels map[IYSChannel][]Purpose
	tally    map[IYSChannel]*syncTally
	failed   map[IYSChannel]error
}

func (run *syncRun) failAll(err error) {
	for ch := range run.channels {
		if run.failed[ch] == nil {
			run.failed[ch] = err
		}
	}
}

// syncIYSStatus reconciles every İYS channel that has a requires_iys
// purpose. A channel that fails is logged and reported; the others still
// complete.
func (s *service) syncIYSStatus(ctx context.Context) error {
	if s.d.IYSFeed == nil && s.d.IYSStates == nil {
		return nil
	}
	run := &syncRun{s: s, feed: s.d.IYSFeed, states: s.d.IYSStates, started: s.now(),
		channels: map[IYSChannel][]Purpose{}, tally: map[IYSChannel]*syncTally{}, failed: map[IYSChannel]error{}}
	purposes, err := s.d.Store.Purposes(ctx)
	if err != nil {
		return err
	}
	for _, p := range purposes {
		// Inactive purposes too: a grant in force would come back with the purpose.
		if p.RequiresIYS && p.IYSChannel != nil {
			run.channels[*p.IYSChannel] = append(run.channels[*p.IYSChannel], p.Code)
			run.tally[*p.IYSChannel] = &syncTally{}
		}
	}
	if len(run.channels) == 0 {
		return nil
	}
	horizon, sweep := run.started, true // without a feed every grant is read on every run (BE-103)
	if run.feed != nil {
		h, due, err := run.pull(ctx)
		horizon, sweep = h, due
		if err != nil {
			run.failAll(err)
		}
	}
	switch {
	case len(run.failed) > 0: // the feed failed: no channel is complete
	case sweep:
		for _, ch := range slices.Sorted(maps.Keys(run.channels)) {
			if err := run.sweepChannel(ctx, ch); err != nil {
				run.failed[ch] = err
			}
		}
		if run.feed != nil && len(run.failed) == 0 {
			if err := s.d.Store.ClearIYSSweep(ctx, run.feed.Feed().Name, run.started); err != nil {
				run.failAll(err)
			}
		}
	case run.states != nil:
		run.checkUnconfirmed(ctx)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	syncedAt := horizon
	if syncedAt.After(run.started) {
		syncedAt = run.started
	}
	var errs []error
	for _, ch := range slices.Sorted(maps.Keys(run.channels)) {
		t := run.tally[ch]
		attrs := []slog.Attr{
			slog.String("channel", string(ch)), slog.Int("checked", t.checked), slog.Int("withdrawn", t.withdrawn),
			slog.Int("conflicts", t.conflicts), slog.Int("in_flight", t.inFlight), slog.Int("missing", t.missing),
			slog.Int("superseded", t.superseded), slog.String("client", s.d.IYS.Name()),
		}
		err := run.failed[ch]
		if err == nil {
			err = s.d.Store.SaveIYSSync(ctx, IYSSync{Channel: ch, SyncedAt: syncedAt, Checked: t.checked, Withdrawn: t.withdrawn})
		}
		if err != nil {
			level := slog.LevelWarn
			attrs = append(attrs, slog.String("error", cut(obs.Scrub(err.Error()), maxLastError)))
			switch {
			case errors.Is(err, errIYSSweepUnavailable): // only a change of adapter (or İYS support) helps
				level = slog.LevelError
				attrs = append(attrs, slog.Bool("alarm", true), slog.String("iys_alarm", IYSAlarmSweepUnavailable))
				s.alarms.sweepUnavailable.Add(1)
			case errors.Is(err, ErrIYSPermanent): // credentials, subscription, a refused request: the next run cannot fix it
				level = slog.LevelError
				attrs = append(attrs, slog.Bool("alarm", true), slog.String("iys_alarm", IYSAlarmReadRejected))
				s.alarms.readRejected.Add(1)
			}
			s.d.Log.LogAttrs(ctx, level, "iys status sync incomplete", attrs...)
			errs = append(errs, fmt.Errorf("consent: İYS status sync of %s: %w", ch, err))
			continue
		}
		level := slog.LevelInfo
		if t.conflicts > 0 || t.missing > 0 {
			level = slog.LevelWarn
		}
		attrs = append(attrs, slog.Time("synced_at", syncedAt))
		s.d.Log.LogAttrs(ctx, level, "iys status sync done", attrs...)
	}
	return errors.Join(errs...)
}

// readError keeps the port error (errors.Is) while its text is scrubbed:
// an adapter's text may repeat an address.
type readError struct {
	msg string
	err error
}

func (e *readError) Error() string { return e.msg }
func (e *readError) Unwrap() error { return e.err }

func iysReadError(what string, err error) error {
	return &readError{msg: what + ": " + cut(obs.Scrub(err.Error()), maxLastError), err: err}
}

// pull applies the change feed from the stored cursor and returns the
// feed's horizon and whether a full reconciliation is due.
func (run *syncRun) pull(ctx context.Context) (time.Time, bool, error) {
	s, info := run.s, run.feed.Feed()
	feed := info.Name
	if !iys.ValidFeed(feed) {
		return time.Time{}, true, fmt.Errorf("consent: İYS adapter %s names no valid change feed", s.d.IYS.Name())
	}
	st, found, err := s.d.Store.IYSFeed(ctx, feed)
	if err != nil {
		return time.Time{}, true, err
	}
	var horizon time.Time
	if st.Horizon != nil {
		horizon = *st.Horizon
	}
	cursor := st.Cursor
	gap := !found || st.Horizon == nil || (info.Window > 0 && horizon.Before(run.started.Add(-info.Window)))
	if gap {
		if err := s.d.Store.MarkIYSSweep(ctx, feed, run.started); err != nil {
			return horizon, true, err
		}
		if st.Horizon != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "iys change feed lost changes: every grant in force is reconciled",
				slog.String("feed", feed), slog.Time("horizon", horizon), slog.Duration("window", info.Window))
		}
		cursor = "" // read the whole window again (withdrawals are idempotent)
	}
	due := gap || st.SweepDueAt != nil
	for range maxFeedPages {
		called := s.now()
		page, err := run.feed.Changes(ctx, cursor)
		if err != nil {
			return horizon, due, iysReadError("İYS change feed", err)
		}
		for _, c := range page.Changes {
			if err := run.applyChange(ctx, c); err != nil {
				return horizon, due, err
			}
		}
		next := cursor
		if page.Cursor != "" {
			next = page.Cursor
		}
		if !iys.ValidCursor(next) {
			return horizon, due, fmt.Errorf("consent: İYS adapter %s returned a malformed cursor", s.d.IYS.Name())
		}
		through := page.Through
		switch {
		case through.IsZero() && page.More: // no claim: keep the horizon
			through = horizon
		case through.IsZero(), through.After(called):
			through = called
		}
		if through.After(horizon) || next != cursor {
			h := horizon
			if through.After(h) {
				h = through
			}
			if err := s.d.Store.AdvanceIYSFeed(ctx, feed, next, h); err != nil {
				return horizon, due, err
			}
			horizon = h
		}
		cursor = next
		if !page.More {
			break
		}
	}
	return horizon, due, nil
}

// applyChange applies one change of the feed: a refusal dated at or after
// the grant in force withdraws it.
func (run *syncRun) applyChange(ctx context.Context, c iys.Change) error {
	ch, known := iysChannels[c.Type]
	purposes, ok := run.channels[ch]
	if !known || !ok || c.UserID == uuid.Nil() || c.State.Status != IYSRefused {
		return nil // a channel Koro does not use, or an approval (never a Koro grant)
	}
	t := run.tally[ch]
	for _, p := range purposes {
		g, found, err := run.s.d.Store.Latest(ctx, c.UserID, p, nil)
		if err != nil {
			return err
		}
		if !found || g.Action != Granted {
			continue
		}
		t.checked++
		if c.State.At.Before(g.OccurredAt.Truncate(time.Second)) {
			t.superseded++ // the newer grant replaces it in İYS once delivered (the unconfirmed check covers the rest)
			continue
		}
		done, err := run.s.withdrawFromIYS(ctx, g, c.State, iysReasonRefused)
		if err != nil {
			return err
		}
		if done {
			t.withdrawn++
		}
	}
	return nil
}

// sweepChannel reads İYS's state of every grant in force of the purposes
// of channel ch, in user ID order (the full reconciliation). Without single
// reads it completes only when there is no grant in force to reconcile.
func (run *syncRun) sweepChannel(ctx context.Context, ch IYSChannel) error {
	if run.states == nil {
		for _, purpose := range run.channels[ch] {
			grants, err := run.s.d.Store.GrantedChains(ctx, purpose, uuid.Nil(), 1)
			if err != nil {
				return err
			}
			if len(grants) > 0 {
				return errIYSSweepUnavailable
			}
		}
		return nil
	}
	for _, purpose := range run.channels[ch] {
		after := uuid.Nil()
		for {
			grants, err := run.s.d.Store.GrantedChains(ctx, purpose, after, IYSStatusBatch)
			if err != nil {
				return err
			}
			if len(grants) == 0 {
				break
			}
			if err := run.judgeGrants(ctx, ch, grants); err != nil {
				return err
			}
			after = grants[len(grants)-1].UserID
			if len(grants) < IYSStatusBatch {
				break
			}
		}
	}
	return nil
}

// checkUnconfirmed reads İYS's state of the grants in force for longer
// than IYSInFlightGrace whose İYS submission is still pending or failed.
func (run *syncRun) checkUnconfirmed(ctx context.Context) {
	s := run.s
	cutoff := run.started.Add(-IYSInFlightGrace)
	grants := map[IYSChannel][]Consent{}
	after := uuid.Nil()
	for {
		subs, err := s.d.Store.DueSubmissions(ctx, run.started, after, dueSubmissionBatch)
		if err != nil {
			run.failAll(err)
			return
		}
		for _, sub := range subs {
			after = sub.ID
			if _, ok := run.channels[sub.Channel]; !ok {
				continue
			}
			c, err := s.d.Store.ConsentByID(ctx, sub.ConsentID)
			switch {
			case errors.Is(err, ErrNotFound):
				continue
			case err != nil:
				run.failAll(err)
				return
			case c.Action != Granted || c.ScopeID != nil || c.OccurredAt.After(cutoff):
				continue
			}
			cur, found, err := s.d.Store.Latest(ctx, c.UserID, c.PurposeCode, nil)
			if err != nil {
				run.failAll(err)
				return
			}
			if found && cur.ID == c.ID {
				grants[sub.Channel] = append(grants[sub.Channel], c)
			}
		}
		if len(subs) < dueSubmissionBatch {
			break
		}
	}
	for _, ch := range slices.Sorted(maps.Keys(grants)) {
		for chunk := range slices.Chunk(grants[ch], IYSStatusBatch) {
			if err := run.judgeGrants(ctx, ch, chunk); err != nil {
				run.failed[ch] = err
				break
			}
		}
	}
}

// judgeGrants reads İYS's current state of the users of grants (at most
// IYSStatusBatch) and withdraws where İYS holds a refusal.
func (run *syncRun) judgeGrants(ctx context.Context, ch IYSChannel, grants []Consent) error {
	ids := make([]uuid.UUID, len(grants))
	for i, g := range grants {
		ids[i] = g.UserID
	}
	if len(ids) > IYSStatusBatch {
		return fmt.Errorf("consent: %d users in one İYS status read (at most %d)", len(ids), IYSStatusBatch)
	}
	states, err := run.states.Statuses(ctx, iysTypes[ch], ids)
	if err != nil {
		return iysReadError("İYS status read", err)
	}
	t := run.tally[ch]
	for _, g := range grants {
		t.checked++
		st, found := states[g.UserID]
		reason := judgeIYS(g, st, found, run.started)
		switch {
		case !found:
			t.missing++
		case reason == "" && st.Status == IYSRefused:
			t.inFlight++
		}
		if reason == "" {
			continue
		}
		done, err := run.s.withdrawFromIYS(ctx, g, st, reason)
		if err != nil {
			return err
		}
		if done {
			t.withdrawn++
			if reason == iysReasonConflict {
				t.conflicts++
			}
		}
	}
	return nil
}

// judgeIYS decides what İYS's current state st means for the grant g in
// force at now: the reason to withdraw, or "" to leave the grant alone.
func judgeIYS(g Consent, st IYSState, found bool, now time.Time) string {
	switch {
	case !found || st.Status != IYSRefused:
		return ""
	case !st.At.Before(g.OccurredAt.Truncate(time.Second)):
		return iysReasonRefused
	case now.Sub(g.OccurredAt) < IYSInFlightGrace:
		return "" // Koro's approval is on its way to İYS
	}
	return iysReasonConflict
}

// withdrawFromIYS appends the İYS withdrawal of grant g when g is still the
// consent in force (the subject lock serialises it with one-click
// unsubscribes and the subject's own acts). It reports whether it wrote.
func (s *service) withdrawFromIYS(ctx context.Context, g Consent, st IYSState, reason string) (bool, error) {
	var out Consent
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.LockSubject(ctx, g.UserID, g.PurposeCode); err != nil {
			return err
		}
		cur, found, err := s.d.Store.Latest(ctx, g.UserID, g.PurposeCode, nil)
		if err != nil || !found || cur.ID != g.ID {
			return err // changed meanwhile: the next run judges the new state
		}
		meta := map[string]any{"iys_reason": reason, "grant_consent_id": g.ID.String()}
		if !st.At.IsZero() {
			meta["iys_status_at"] = st.At.UTC().Format(time.RFC3339)
		}
		if reIYSSourceCode.MatchString(st.Source) {
			meta["iys_source"] = st.Source
		}
		out, err = s.recordWith(ctx, RecordInput{UserID: g.UserID, Purpose: g.PurposeCode, Action: Withdrawn, Channel: ChannelIYS},
			authz.SystemActor(StatusSyncJobName), meta)
		return err
	})
	if err != nil || out.ID == uuid.Nil() {
		return false, err
	}
	if reason == iysReasonConflict {
		s.alarms.conflict.Add(1)
		s.d.Log.LogAttrs(ctx, slog.LevelError, "iys refusal older than a delivered grant: consent withdrawn",
			slog.Bool("alarm", true), slog.String("iys_alarm", IYSAlarmConflict), slog.String("consent_id", out.ID.String()),
			slog.String("grant_consent_id", g.ID.String()), slog.String("purpose", string(g.PurposeCode)))
	}
	return true, nil
}

// iysFresh is the send-time hold (BE-103): with CONSENT_IYS_SYNC_MAX_AGE set,
// an İYS purpose answers only while its channel was reconciled within it.
func (s *service) iysFresh(ctx context.Context, p PurposeDef) error {
	if s.d.IYSSyncMaxAge <= 0 || !p.RequiresIYS || p.IYSChannel == nil {
		return nil
	}
	st, found, err := s.d.Store.IYSSync(ctx, *p.IYSChannel)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("%w (%s: never reconciled)", ErrIYSSyncStale, *p.IYSChannel)
	}
	if age := s.now().Sub(st.SyncedAt); age > s.d.IYSSyncMaxAge {
		return fmt.Errorf("%w (%s: last reconciled %s ago, at most %s)", ErrIYSSyncStale, *p.IYSChannel,
			age.Truncate(time.Minute), s.d.IYSSyncMaxAge)
	}
	return nil
}
