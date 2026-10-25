package netgsm

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"koro/internal/platform/iys"
	"koro/internal/platform/queue"
)

// The backup check (BE-110): Netgsm's webhook has no documented retry or
// replay, so a delivery Koro missed (an outage, a wrong token) would leave
// a refusal given in İYS outside the ledger. Every ten minutes the job
// reads İYS's state of a slice of the users with a grant in force through
// /iys/search (Recheck) and adds every refusal İYS holds to the feed as a
// change, which consent.iys_status_sync then applies like a pushed one (a
// refusal dated after the grant withdraws it; an older one is superseded
// by the grant). The slices rotate: with n users and perHour checks an
// hour every user is read once every about n/perHour hours. The queries of
// a run are spaced (RecheckSpacing: half of Netgsm's per-minute budget) so
// that uploads keep the rest.

// RecheckJobName is the scheduled backup check.
const RecheckJobName = "consent.iys_netgsm_recheck"

// RecheckPeriod is the period of the backup check.
const RecheckPeriod = 10 * time.Minute

const (
	recheckCron    = "*/10 * * * *"
	recheckUnique  = 9 * time.Minute
	recheckTimeout = 9 * time.Minute
	recheckBudget  = 8 * time.Minute // a run's queries end within it
)

// RecheckResult counts one backup check (logged; never personal data).
type RecheckResult struct {
	// Checked users whose state İYS answered (with or without a record).
	Checked int
	// Refusals added to the feed that it did not hold (by İYS's transaction
	// ID: the webhook missed them, or İYS did not name the transaction).
	Refusals int
}

// Recheck reads İYS's state of users for t and adds the refusals İYS
// holds to the change feed. A refusal of Koro's own (an "HS_*" source) is
// in the ledger already and one without a date cannot be judged: both are
// left out. It stops at the first error (a spent budget is
// iys.ErrThrottled) and returns what it did.
func (c *Client) Recheck(ctx context.Context, t iys.Type, users []uuid.UUID) (RecheckResult, error) {
	var res RecheckResult
	if !iys.ValidType(t) {
		return res, fmt.Errorf("%w: unknown İYS type", iys.ErrRejected)
	}
	for _, u := range users {
		st, tx, found, err := c.query(ctx, t, u)
		switch {
		case errors.Is(err, errNoAddress):
			continue // an erased account: nothing to read
		case err != nil:
			return res, err
		}
		res.Checked++
		if !found || st.Status != iys.Refused || st.At.IsZero() || strings.HasPrefix(st.Source, "HS_") {
			continue
		}
		e := Event{Kind: KindChange, UserID: u, Type: t, Status: iys.Refused, Source: st.Source, ConsentAt: st.At,
			RecordedAt: st.At, TransactionID: tx, ReceivedAt: c.c.Clock.Now().UTC()}
		e.Key = eventKey(c.feed, e) // the webhook's key when İYS names the transaction
		n, err := c.c.Store.Append(ctx, c.feed, []Event{e})
		if err != nil {
			return res, err
		}
		res.Refusals += n
	}
	return res, nil
}

// RecheckDue picks the users the backup check reads at now: the slice of
// the current period of a rotation of ceil(len(users)/perPeriod) periods
// (at most perPeriod), in ID order. The assignment changes every rotation,
// so a user left out by the cap is picked again soon.
func RecheckDue(users []uuid.UUID, now time.Time, period time.Duration, perPeriod int) []uuid.UUID {
	if perPeriod <= 0 || period <= 0 || len(users) == 0 {
		return nil
	}
	slots := uint64((len(users) + perPeriod - 1) / perPeriod) //nolint:gosec // G115: positive by the guard above
	n := uint64(max(now.UnixNano(), 0)) / uint64(period)
	rotation, slot := n/slots, n%slots
	var out []uuid.UUID
	for _, u := range users {
		h := fnv.New64a()
		h.Write(u[:])
		h.Write([]byte(strconv.FormatUint(rotation, 10)))
		if h.Sum64()%slots == slot {
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if len(out) > perPeriod {
		out = out[:perPeriod]
	}
	return out
}

// RecheckJob is the backup check (every RecheckPeriod) of the users list
// returns (the users with a grant in force of an İYS purpose of type t;
// cmd/koro reads them from consent's ledger): about perHour queries an
// hour, spaced by RecheckSpacing and at most what fits a run. A spent
// budget ends the run early without an error (the next rotation reads the
// rest).
func (c *Client) RecheckJob(t iys.Type, perHour int, list func(ctx context.Context) ([]uuid.UUID, error)) queue.Job {
	perRun := (perHour*int(RecheckPeriod/time.Minute) + 59) / 60
	if fit := int(recheckBudget / max(c.c.RecheckSpacing, time.Millisecond)); perRun > fit {
		perRun = fit
	}
	return queue.Job{
		Name: RecheckJobName, Cron: recheckCron, Queue: queue.Default, Unique: recheckUnique, Timeout: recheckTimeout,
		Handle: func(ctx context.Context) error {
			users, err := list(ctx)
			if err != nil {
				return err
			}
			due := RecheckDue(users, c.c.Clock.Now(), RecheckPeriod, perRun)
			res, err := c.recheckPaced(ctx, t, due)
			level := slog.LevelInfo
			attrs := []slog.Attr{slog.Int("candidates", len(users)), slog.Int("due", len(due)), slog.Int("checked", res.Checked),
				slog.Int("refusals", res.Refusals)}
			switch {
			case errors.Is(err, iys.ErrThrottled):
				level, err = slog.LevelWarn, nil
				attrs = append(attrs, slog.String("stopped", "budget spent"))
			case err != nil:
				level = slog.LevelWarn
				attrs = append(attrs, slog.String("error", scrub(err.Error())))
			}
			if res.Refusals > 0 {
				level = slog.LevelWarn // a refusal the webhook did not deliver: check the webhook
				attrs = append(attrs, slog.Bool("webhook_missed", true))
			}
			c.c.Log.LogAttrs(ctx, level, "iys netgsm recheck done", attrs...)
			return err
		},
	}
}

// recheckPaced is Recheck one user at a time, RecheckSpacing apart (real
// time: the budget of Netgsm is real time too).
func (c *Client) recheckPaced(ctx context.Context, t iys.Type, users []uuid.UUID) (RecheckResult, error) {
	var res RecheckResult
	for i, u := range users {
		if i > 0 {
			timer := time.NewTimer(c.c.RecheckSpacing)
			select {
			case <-ctx.Done():
				timer.Stop()
				return res, ctx.Err()
			case <-timer.C:
			}
		}
		r, err := c.Recheck(ctx, t, []uuid.UUID{u})
		res.Checked, res.Refusals = res.Checked+r.Checked, res.Refusals+r.Refusals
		if err != nil {
			return res, err
		}
	}
	return res, nil
}
