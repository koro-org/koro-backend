package outreach

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
	"koro/internal/students"
)

// The run of a dispatch (BE-035; 01 §6.2 outreach.dispatch_scheduled, §3.7
// outreach.dispatch.run). One worker claims the dispatch (scheduled →
// running; a claim older than the lease may be taken over), walks its
// recipients in batches of dispatchBatch and hands one notification per
// recipient and channel to notify, whose dedupe key makes a second run
// harmless; then it writes the published counts. A dispatch whose post is
// not published yet waits (back to scheduled: post.published or the
// resume job starts it); a post that left the feed, or an audience that
// fell below 10, ends the dispatch without sending and gives a company's
// unit back.

// Tuning of the run.
const (
	// dispatchBatch is the number of recipients checked and notified per
	// batch (01 §7: at most 1 000 rows per step); every batch renews the
	// claim.
	dispatchBatch = 1000
	// resumeDelay is how old a scheduled dispatch must be before the resume
	// job runs it: its outbox event normally comes first.
	resumeDelay = 2 * time.Minute
	// resumeBatch is the number of dispatches the resume job reads at once.
	resumeBatch = 100
)

// Templates of the dispatch (internal/notify/templates/outreach): the
// informational notice of a club, university or the platform (preference
// feed_follows) and the commercial message (consent; push and e-mail).
const (
	templateNotice     = "outreach.announcement"
	templateCommercial = "outreach.commercial"
)

// Reasons a dispatch ends without sending (last_error: problem codes and
// fixed words, never personal data).
const (
	reasonPostGone        = "outreach.post_gone"
	reasonPostUnpublished = "outreach.post_not_published"
)

// errPermanentSend marks a notify error that no retry fixes (an invalid
// message or template, commercial messages switched off): the dispatch
// fails instead of being retried.
var errPermanentSend = errors.New("outreach: notification cannot be sent")

// handleDispatchScheduled is outreach.run_dispatch
// (outreach.dispatch_scheduled, O).
func (s *service) handleDispatchScheduled(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OutreachDispatchScheduled](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.run(ctx, p.DispatchID)
}

// handlePostPublished is outreach.start_dispatch (post.published, A): the
// waiting dispatch of the post runs. Most posts have none.
func (s *service) handlePostPublished(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PostPublished](e)
	if err != nil {
		return queue.Permanent(err)
	}
	d, err := s.d.Store.DispatchOfPost(ctx, p.OrgID, p.PostID, false)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case d.Status != DispatchScheduled:
		return nil
	}
	return s.run(ctx, d.ID)
}

// resumeDispatches is outreach.resume_dispatches: the scheduled dispatches
// whose event or post.published was lost (A is at most once) and the runs
// a crashed worker left behind. Each dispatch runs in turn; a failing one
// does not stop the others.
func (s *service) resumeDispatches(ctx context.Context) error {
	after := uuid.Nil()
	var errs []error
	for ctx.Err() == nil {
		rows, err := s.d.Store.PendingDispatches(ctx, s.now().Add(-resumeDelay), s.d.Lease, after, resumeBatch)
		if err != nil {
			return fmt.Errorf("%s: %w", ResumeDispatchesJobName, err)
		}
		if len(rows) == 0 {
			break
		}
		after = rows[len(rows)-1].ID
		for _, r := range rows {
			if err := s.run(ctx, r.ID); err != nil {
				s.d.Log.LogAttrs(ctx, slog.LevelWarn, "dispatch resume failed",
					slog.String("dispatch_id", r.ID.String()), slog.String("org_id", r.OrgID.String()),
					slog.String("err", err.Error()))
				errs = append(errs, err)
			}
		}
		if len(rows) < resumeBatch {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(errs...)
}

// run claims dispatch id and runs it. Nothing happens to a finished
// dispatch or one running elsewhere. A transient error gives the claim back
// (the retry or the resume job runs it again from the start; notify's dedupe
// keys skip what was sent) and is returned.
func (s *service) run(ctx context.Context, id uuid.UUID) error {
	d, ok, err := s.d.Store.ClaimDispatch(ctx, id, s.now(), s.d.Lease)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil // deleted meanwhile (its post was withdrawn)
	case err != nil:
		return err
	case !ok:
		return nil
	}
	if err := s.runClaimed(ctx, d); err != nil {
		if _, _, uerr := s.d.Store.TransitionDispatch(context.WithoutCancel(ctx), d.OrgID, d.ID, DispatchRunning,
			DispatchScheduled, nil, time.Time{}); uerr != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelError, "dispatch claim not released",
				slog.String("dispatch_id", d.ID.String()), slog.String("err", uerr.Error()))
		}
		return fmt.Errorf("%s %s: %w", RunDispatchTaskName, d.ID, err)
	}
	return nil
}

// runClaimed runs a claimed dispatch to its end, or back to scheduled when
// its post is not published yet.
func (s *service) runClaimed(ctx context.Context, d Dispatch) error {
	post, err := s.d.Posts.DispatchPost(ctx, d.PostID)
	switch {
	case errors.Is(err, ErrNotFound):
		return s.abandon(ctx, d, DispatchCancelled, reasonPostGone)
	case err != nil:
		return err
	case post.OrgID != d.OrgID:
		return s.abandon(ctx, d, DispatchCancelled, reasonPostGone) // never: the composite FK (06 H-38)
	}
	switch post.Status {
	case PostPublished:
	case PostDraft, PostPendingReview:
		// Not yet: post.published (or the resume job) starts it.
		_, _, err := s.d.Store.TransitionDispatch(ctx, d.OrgID, d.ID, DispatchRunning, DispatchScheduled, nil, time.Time{})
		return err
	default: // hidden, removed: the post no longer reaches anyone
		return s.abandon(ctx, d, DispatchCancelled, reasonPostUnpublished)
	}
	org, err := s.d.Orgs.Get(ctx, d.OrgID)
	switch {
	case errors.Is(err, orgs.ErrNotFound):
		return s.abandon(ctx, d, DispatchCancelled, reasonPostGone)
	case err != nil:
		return err
	case org.Status == orgs.StatusSuspended:
		return s.abandon(ctx, d, DispatchCancelled, orgs.ErrOrgSuspended.Code())
	case org.Status != orgs.StatusActive:
		return s.abandon(ctx, d, DispatchCancelled, orgs.ErrInvalidState.Code())
	}
	// A sensitive club's membership is never counted (03 Y2; BE-030
	// Compile): its notice to its own members keeps no counts at all.
	uncounted := false
	if d.memberList() {
		sensitive, err := s.d.Catalog.SensitiveOrgIDs(ctx, []uuid.UUID{d.OrgID})
		if err != nil {
			return err
		}
		uncounted = sensitive[d.OrgID]
	}
	t, err := s.walk(ctx, d, org)
	switch {
	case errors.Is(err, students.ErrAudienceTooSmall):
		// The audience fell below 10 since the post was compiled: nobody
		// is notified, and the size is never shown (k-anonymity).
		return s.abandon(ctx, d, DispatchFailed, ErrAudienceTooSmall.Code())
	case errors.Is(err, problem.ErrValidation):
		return s.abandon(ctx, d, DispatchFailed, problem.ErrValidation.Code()) // a malformed stored targeting
	case errors.Is(err, errPermanentSend):
		return s.abandon(ctx, d, DispatchFailed, notify.ErrInvalidMessage.Code())
	case err != nil:
		return err
	}
	return s.complete(ctx, d, t, uncounted)
}

// tally is the exact count of a run. It never leaves the run: complete
// stores PublicEstimate of each (06 H-61, T-260) and nothing logs it.
type tally struct {
	eligible  int  // active adult recipients the walk reached
	consented int  // of them, reachable on at least one channel (the consent of a commercial channel)
	sent      int  // of them, notified on at least one channel
	failed    bool // a delivery failed (provider): logged without a number
}

// walk notifies the recipients of d in batches: the club's members for a
// club's notice to its own members (T-258), the audience (students'
// Stream, k ≥ 10, sensitive clubs never) otherwise.
func (s *service) walk(ctx context.Context, d Dispatch, org orgs.OrgSummary) (tally, error) {
	var t tally
	batch := make([]uuid.UUID, 0, dispatchBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.deliverBatch(ctx, d, org, batch, &t)
		batch = batch[:0]
		if err != nil {
			return err
		}
		return s.d.Store.TouchDispatch(ctx, d.ID) // the claim holds while batches go out
	}
	add := func(id uuid.UUID) error {
		batch = append(batch, id)
		if len(batch) < dispatchBatch {
			return nil
		}
		return flush()
	}
	if d.memberList() {
		ids, err := s.d.Members.ActiveMemberIDs(ctx, d.OrgID)
		if err != nil {
			return t, err
		}
		slices.SortFunc(ids, uuid.UUID.Compare)
		for _, id := range slices.Compact(ids) {
			if err := add(id); err != nil {
				return t, err
			}
		}
	} else if err := s.d.Recipients.Stream(ctx, students.AudienceSpec{Keys: d.AudienceKeys}, add); err != nil {
		return t, err
	}
	return t, flush()
}

// deliverBatch notifies one batch: only active adult accounts (a minor
// never receives a dispatch, whatever the walk said: T-135), on a
// commercial dispatch only on the channels whose commercial consent is in
// force (6563; notify checks again), one notification per channel.
func (s *service) deliverBatch(ctx context.Context, d Dispatch, org orgs.OrgSummary, ids []uuid.UUID, t *tally) error {
	users, err := s.d.Users.GetMany(ctx, ids)
	if err != nil {
		return err
	}
	adults := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if u, ok := users[id]; ok && u.Status == identity.StatusActive && !u.IsMinor {
			adults = append(adults, id)
		}
	}
	t.eligible += len(adults)
	if len(adults) == 0 {
		return nil
	}
	chans := d.ChannelList()
	reach := make(map[Channel]map[uuid.UUID]bool, len(chans))
	for _, ch := range chans {
		set := map[uuid.UUID]bool{}
		if d.Commercial {
			purpose, ok := commercialPurpose(ch)
			if !ok {
				continue // in-app: never commercial (refused when scheduled)
			}
			ok2, err := s.d.Consents.FilterActive(ctx, adults, purpose)
			if err != nil {
				return err
			}
			for _, id := range ok2 {
				set[id] = true
			}
		} else {
			for _, id := range adults {
				set[id] = true // informational (T-16): no consent needed
			}
		}
		reach[ch] = set
	}
	for _, id := range adults {
		var consented, sent bool
		for _, ch := range chans {
			if !reach[ch][id] {
				continue
			}
			consented = true
			out, err := s.send(ctx, d, org, users[id], ch)
			if err != nil {
				return err
			}
			sent = sent || out == sendOK
			t.failed = t.failed || out == sendFailed
		}
		if consented {
			t.consented++
		}
		if sent {
			t.sent++
		}
	}
	return nil
}

// commercialPurpose is the consent purpose of a commercial channel.
func commercialPurpose(ch Channel) (consent.Purpose, bool) {
	switch ch {
	case ChannelPush:
		return consent.PurposeCommercialPush, true
	case ChannelEmail:
		return consent.PurposeCommercialEmail, true
	}
	return "", false
}

// dedupeKey is the notify dedupe key of a recipient's notification on a
// channel: the same on every run of the dispatch, so a retry never
// notifies twice (02 §6.1 notification_deliveries_dedupe_key_uq). IDs
// only, no personal data.
func dedupeKey(dispatchID, userID uuid.UUID, ch Channel) string {
	return "dispatch:" + dispatchID.String() + ":" + userID.String() + ":" + string(ch)
}

// sendOutcome is what became of one notification.
type sendOutcome int

const (
	sendOK      sendOutcome = iota // sent (or an earlier attempt is still sending it)
	sendSkipped                    // no address any more (an erased account)
	sendFailed                     // refused or failed at the provider
)

// send hands one notification to notify. An error stops the run:
// errPermanentSend (wrapped) when no retry can fix it, anything else is
// transient.
func (s *service) send(ctx context.Context, d Dispatch, org orgs.OrgSummary, u identity.UserSummary, ch Channel) (sendOutcome, error) {
	uid, src := u.ID, d.OrgID
	m := notify.Message{
		UserID: &uid, Channel: notify.Channel(ch), Category: notify.CategoryTransactional, Template: templateNotice,
		Locale: u.Locale, DedupeKey: dedupeKey(d.ID, u.ID, ch), LinkPath: d.DeepLink,
	}
	if d.Commercial {
		m.Category, m.Template = notify.CategoryCommercial, templateCommercial
	}
	switch ch {
	case ChannelEmail:
		c, err := s.d.Users.Contact(ctx, u.ID)
		switch {
		case errors.Is(err, identity.ErrNotFound):
			return sendSkipped, nil // erased meanwhile
		case err != nil:
			return sendFailed, err
		}
		m.ToEmail, m.Locale = c.Email, c.Locale
		title := d.TitleSnapshot
		if title == "" {
			title = org.Name
		}
		m.Data = map[string]any{"title": title, "org_name": org.Name, "url": s.d.LinkBaseURL + d.DeepLink}
		if d.Commercial {
			// The recipient's own link (BE-062). Without one the mail
			// cannot go out lawfully; nothing a retry can fix.
			if s.d.Unsubscribe == nil {
				return sendFailed, fmt.Errorf("%w: commercial e-mail without unsubscribe links", errPermanentSend)
			}
			link, err := s.d.Unsubscribe.UnsubscribeURL(u.ID, consent.PurposeCommercialEmail)
			if err != nil {
				return sendFailed, fmt.Errorf("%w: unsubscribe link: %w", errPermanentSend, err)
			}
			m.UnsubscribeURL = link
		}
	default:
		// Fixed texts: the data holds opaque IDs only (no personal data).
		m.Data = map[string]any{"post_id": d.PostID, "dispatch_id": d.ID}
		if ch == ChannelInApp {
			m.SourceOrgID = &src
		}
	}
	_, err := s.d.Sender.Send(ctx, m)
	switch {
	case err == nil, errors.Is(err, notify.ErrDeliveryInFlight):
		return sendOK, nil
	case errors.Is(err, notify.ErrDeliveryRejected), errors.Is(err, notify.ErrDeliveryFailed):
		return sendFailed, nil
	case errors.Is(err, notify.ErrInvalidMessage), errors.Is(err, notify.ErrCommercialDisabled),
		errors.Is(err, notify.ErrNotInWorker):
		return sendFailed, fmt.Errorf("%w: %w", errPermanentSend, err)
	}
	return sendFailed, err
}

// complete stores the published counts of a run (none when uncounted: a
// sensitive club's members) and audits it.
func (s *service) complete(ctx context.Context, d Dispatch, t tally, uncounted bool) error {
	now := s.now()
	counts := publicCounts(t.eligible, t.consented, t.sent)
	if uncounted {
		counts = DispatchCounts{}
	}
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		ok, err := s.d.Store.CompleteDispatch(ctx, d.OrgID, d.ID, counts, now)
		if err != nil || !ok {
			return err // !ok: taken over after an expired claim; the other run writes
		}
		return s.d.Audit.Log(ctx, auditDispatch(authz.SystemActor(RunDispatchTaskName), authz.OutreachDispatchRun, d,
			map[string]any{"status": string(DispatchCompleted), "channels": []string(d.Channels), "commercial": d.Commercial}))
	})
	if err != nil {
		return err
	}
	if t.failed {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "dispatch completed with failed deliveries",
			slog.String("dispatch_id", d.ID.String()), slog.String("org_id", d.OrgID.String()))
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "dispatch completed",
		slog.String("dispatch_id", d.ID.String()), slog.String("org_id", d.OrgID.String()))
	return nil
}

// abandon ends a claimed dispatch without (further) sending: status to
// (cancelled or failed), the reason, a company's unit back, the audit
// record — one transaction.
func (s *service) abandon(ctx context.Context, d Dispatch, to DispatchStatus, reason string) error {
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		next, ok, err := s.d.Store.TransitionDispatch(ctx, d.OrgID, d.ID, DispatchRunning, to, &reason, s.now())
		if err != nil || !ok {
			return err
		}
		if err := s.releaseUnit(ctx, next); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, auditDispatch(authz.SystemActor(RunDispatchTaskName), authz.OutreachDispatchRun, next,
			map[string]any{"status": string(to), "reason": reason, "unit_released": next.EntitlementUsageID != nil}))
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "dispatch ended without sending",
		slog.String("dispatch_id", d.ID.String()), slog.String("org_id", d.OrgID.String()),
		slog.String("status", string(to)), slog.String("reason", reason))
	return nil
}
