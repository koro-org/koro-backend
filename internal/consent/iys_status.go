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
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

// İYS status reconciliation (BE-103; 00 T-312). A recipient may refuse
// commercial messages in İYS itself (İYS web, mobile, call centre) or
// through a channel only İYS records; Koro's gates read the ledger alone
// (FilterActive), so such a refusal must reach the ledger. Under 6563 the
// state İYS holds is what a commercial message is checked against.
//
// consent.iys_status_sync reads İYS's current state for every user whose
// consent to an İYS purpose is in force and appends a withdrawal (channel
// iys, the system actor consent.iys_status_sync, audited) where İYS holds
// a refusal:
//
//   - İYS refusal dated at or after the grant (İYS keeps seconds): the
//     recipient refused after consenting → withdrawn ("refused").
//   - İYS refusal older than the grant: Koro's approval may still be on its
//     way (submission pending or failed, İYS processing it). Within
//     IYSInFlightGrace nothing changes; after it İYS evidently did not take
//     the newer approval, and İYS's state governs sending → withdrawn
//     ("conflict", logged as an alarm for the DPO).
//   - İYS approval, or no İYS record: nothing changes (an İYS approval never
//     becomes a Koro grant; a missing record is counted and logged).
//
// The run is idempotent: a withdrawn chain is no longer a grant, and every
// withdrawal re-reads the chain under the subject lock, so a grant or a
// withdrawal recorded meanwhile is left to the next run. A channel whose
// grants were all read writes its iys_status_syncs row (synced_at = the
// start of the run); the optional hold reads it.
//
// The port is IYSStatusReader. The İYS HTTP adapter (IYSHTTP, BE-055) does
// not implement it yet: Koro's adapter knows İYS's sign-in and consent-add
// calls only, and the state query / change notification of İYS is to be
// built from İYS's integration documentation (00 T-354). Until then only
// FakeIYS reads states (tests, dev), consent.iys_status_sync is not
// scheduled with the real adapter, and CONSENT_IYS_SYNC_MAX_AGE refuses to
// start without a reader.

// IYSStatus is İYS's consent status of a recipient on a channel.
type IYSStatus string

// İYS statuses.
const (
	IYSApproved IYSStatus = "ONAY"
	IYSRefused  IYSStatus = "RET"
)

// IYSState is İYS's current consent record of a recipient on a channel.
type IYSState struct {
	Status IYSStatus
	// At is the record's consent date (İYS keeps seconds).
	At time.Time
	// Source is İYS's source code of the record (e.g. "HS_WEB" for a brand
	// channel, "IYS_WEB" for İYS's own); audit only, kept when it is a
	// plain code.
	Source string
}

// IYSStatusReader reads İYS's current consent states (consent.iys_status_sync).
// The adapter resolves each user's recipient address itself (through
// IYSRecipients, like Submit); the address is used for the request only and
// never stored, logged or returned.
type IYSStatusReader interface {
	// Statuses returns İYS's current state of the recipients of userIDs on
	// channel ch, keyed by user ID; at most IYSStatusBatch users. A user
	// İYS has no record of, or whose address is gone, is absent. An error
	// fails the batch: the run ends incomplete and is repeated.
	Statuses(ctx context.Context, ch IYSChannel, userIDs []uuid.UUID) (map[uuid.UUID]IYSState, error)
}

// ErrIYSSyncStale is FilterActive's answer for an İYS purpose while the
// send-time hold is on (CONSENT_IYS_SYNC_MAX_AGE) and the channel's last
// complete İYS reconciliation is older than that, or never ran. A refusal
// given in İYS meanwhile would not be in the ledger yet: callers keep the
// message for later (outreach's dispatch goes back to scheduled and is
// retried; BE-103).
var ErrIYSSyncStale = errors.New("consent: İYS consent states are not reconciled recently enough; commercial messages are held")

const (
	// StatusSyncJobName is consent.iys_status_sync (BE-103): hourly at :20.
	StatusSyncJobName = "consent.iys_status_sync"
	// IYSStatusBatch is the number of users per Statuses call (Koro's own
	// chunk; the adapter may split further).
	IYSStatusBatch = 100
	// IYSInFlightGrace is how long a grant newer than İYS's refusal is
	// assumed to be on its way to İYS (delivery, retries, İYS processing;
	// 6563 gives the brand 3 business days to register a consent).
	IYSInFlightGrace = 72 * time.Hour

	statusSyncCron    = "20 * * * *"
	statusSyncUnique  = 50 * time.Minute
	statusSyncTimeout = 30 * time.Minute

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
	checked, withdrawn, conflicts, inFlight, missing int
}

// syncIYSStatus reconciles every İYS channel that has a requires_iys
// purpose. A channel that fails is logged and reported; the others still
// complete.
func (s *service) syncIYSStatus(ctx context.Context) error {
	if s.d.IYSStatus == nil {
		return nil
	}
	started := s.now()
	purposes, err := s.d.Store.Purposes(ctx)
	if err != nil {
		return err
	}
	byChannel := map[IYSChannel][]Purpose{}
	for _, p := range purposes {
		// Inactive purposes too: a grant in force would come back with the purpose.
		if p.RequiresIYS && p.IYSChannel != nil {
			byChannel[*p.IYSChannel] = append(byChannel[*p.IYSChannel], p.Code)
		}
	}
	var errs []error
	for _, ch := range slices.Sorted(maps.Keys(byChannel)) {
		t, err := s.syncChannel(ctx, ch, byChannel[ch], started)
		if err == nil {
			err = s.d.Store.SaveIYSSync(ctx, IYSSync{Channel: ch, SyncedAt: started, Checked: t.checked, Withdrawn: t.withdrawn})
		}
		attrs := []slog.Attr{
			slog.String("channel", string(ch)), slog.Int("checked", t.checked), slog.Int("withdrawn", t.withdrawn),
			slog.Int("conflicts", t.conflicts), slog.Int("in_flight", t.inFlight), slog.Int("missing", t.missing),
		}
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			attrs = append(attrs, slog.String("error", cut(obs.Scrub(err.Error()), maxLastError)))
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "iys status sync incomplete", attrs...)
			errs = append(errs, fmt.Errorf("consent: İYS status sync of %s: %w", ch, err))
			continue
		}
		level := slog.LevelInfo
		if t.conflicts > 0 || t.missing > 0 {
			level = slog.LevelWarn
		}
		s.d.Log.LogAttrs(ctx, level, "iys status sync done", attrs...)
	}
	return errors.Join(errs...)
}

// syncChannel reads İYS's state of every grant in force of the purposes of
// channel ch, in user ID order, and withdraws where İYS holds a refusal.
func (s *service) syncChannel(ctx context.Context, ch IYSChannel, purposes []Purpose, started time.Time) (syncTally, error) {
	var t syncTally
	for _, purpose := range purposes {
		after := uuid.Nil()
		for {
			grants, err := s.d.Store.GrantedChains(ctx, purpose, after, IYSStatusBatch)
			if err != nil {
				return t, err
			}
			if len(grants) == 0 {
				break
			}
			ids := make([]uuid.UUID, len(grants))
			for i, g := range grants {
				ids[i] = g.UserID
			}
			states, err := s.d.IYSStatus.Statuses(ctx, ch, ids)
			if err != nil {
				// The adapter's text may repeat an address: scrubbed.
				return t, errors.New("İYS status read: " + cut(obs.Scrub(err.Error()), maxLastError))
			}
			for _, g := range grants {
				t.checked++
				st, found := states[g.UserID]
				reason := judgeIYS(g, st, found, started)
				switch {
				case !found:
					t.missing++
				case reason == "" && st.Status == IYSRefused:
					t.inFlight++
				}
				if reason == "" {
					continue
				}
				done, err := s.withdrawFromIYS(ctx, g, st, reason)
				if err != nil {
					return t, err
				}
				if done {
					t.withdrawn++
					if reason == iysReasonConflict {
						t.conflicts++
					}
				}
			}
			after = grants[len(grants)-1].UserID
			if len(grants) < IYSStatusBatch {
				break
			}
		}
	}
	return t, nil
}

// judgeIYS decides what İYS's state st means for the grant g in force at
// now: the reason to withdraw, or "" to leave the grant alone.
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
		meta := map[string]any{
			"iys_reason": reason, "iys_status_at": st.At.UTC().Format(time.RFC3339), "grant_consent_id": g.ID.String(),
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
		s.d.Log.LogAttrs(ctx, slog.LevelError, "iys refusal older than a delivered grant: consent withdrawn",
			slog.Bool("alarm", true), slog.String("consent_id", out.ID.String()), slog.String("grant_consent_id", g.ID.String()),
			slog.String("purpose", string(g.PurposeCode)))
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
