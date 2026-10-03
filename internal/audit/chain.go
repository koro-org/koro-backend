package audit

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/queue"
)

// Scheduled jobs of audit (01 §7).
const (
	JobChain       = "audit.chain"        // every 10 s, critical
	JobDailyRoot   = "audit.daily_root"   // 00:15 Europe/Istanbul, default
	JobVerifyChain = "audit.verify_chain" // 05:00 Europe/Istanbul, low
)

// Job tuning.
const (
	chainBatch       = 1000 // rows per transaction (01 §7: ≤ 1000)
	chainMaxBatches  = 50   // batches per run; the next run continues
	chainLagAlarm    = 10 * time.Minute
	rootBackfillDays = 31 // missed days a daily_root run catches up
	verifyRecentDays = 2  // audit.verify_chain: the last two days and today
)

// errInTx: the chain switches its transaction's role, so it must own it.
var errInTx = errors.New("audit: the chain job must not run inside another transaction")

func (s *service) jobs() []queue.Job {
	return []queue.Job{
		{Name: JobChain, Cron: "@every 10s", Queue: queue.Critical, Unique: 9 * time.Second, Timeout: time.Minute,
			Handle: func(ctx context.Context) error { _, err := s.chain(ctx); return err }},
		{Name: JobDailyRoot, Cron: "15 0 * * *", Queue: queue.Default, Unique: 23 * time.Hour, Handle: s.dailyRoots},
		{Name: JobVerifyChain, Cron: "0 5 * * *", Queue: queue.Low, Unique: 23 * time.Hour, Timeout: 30 * time.Minute,
			Handle: s.verifyRecent},
	}
}

// chain links every unchained row (in seq order) to the chain and returns
// how many it linked. Batches run in separate transactions as
// audit_chainer under the chain lock.
func (s *service) chain(ctx context.Context) (int, error) {
	if err := authz.Can(ctx, authz.SystemActor(JobChain), authz.AuditChainAppend, authz.Resource{Type: authz.TypeAuditLog}); err != nil {
		return 0, err
	}
	if db.InTx(ctx) {
		return 0, errInTx
	}
	s.chainMu.Lock()
	defer s.chainMu.Unlock()
	total := 0
	for range chainMaxBatches {
		n, err := s.chainBatch(ctx)
		total += n
		if err != nil {
			return total, err
		}
		if n < chainBatch {
			break
		}
	}
	return total, nil
}

func (s *service) chainBatch(ctx context.Context) (int, error) {
	n := 0
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		head, err := s.d.Store.BeginChain(ctx)
		if err != nil {
			return err
		}
		rows, err := s.d.Store.Unchained(ctx, chainBatch)
		if err != nil {
			return err
		}
		at := s.now()
		for _, r := range rows {
			h, err := RowHash(head.Hash, r)
			if err != nil {
				return err
			}
			next := Link{Seq: head.Seq + 1, Hash: h}
			if err := s.d.Store.SetChain(ctx, r.ID, next.Seq, head.Hash, h, at); err != nil {
				return err
			}
			head = next
		}
		n = len(rows)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// rootDoc is the archived copy of a daily root.
type rootDoc struct {
	Version      int    `json:"version"`
	Day          string `json:"day"`
	LastChainSeq int64  `json:"last_chain_seq"`
	RootHash     string `json:"root_hash"` // hex
	Signature    string `json:"signature"` // base64
	KeyID        string `json:"key_id"`
}

func archiveKey(day civil.Date) string { return "audit-roots/" + day.String() + ".json" }

// dailyRoots signs the chain head of every finished day without a root,
// from the day after the newest root (at most rootBackfillDays back) to
// yesterday (Europe/Istanbul). It is idempotent: a day that already has a
// root is skipped, an archived copy is written once.
func (s *service) dailyRoots(ctx context.Context) error {
	err := s.makeRoots(ctx)
	if err != nil {
		s.alarm(ctx, Alarm{Kind: AlarmRootFailed, Detail: err.Error()})
	}
	return err
}

func (s *service) makeRoots(ctx context.Context) error {
	switch {
	case s.d.Signer == nil:
		return ErrSignerNotConfigured
	case s.d.Archive == nil:
		return ErrArchiveNotConfigured
	}
	yesterday := civil.Today(s.now()).AddDays(-1)
	last, ok, err := s.d.Store.LastRoot(ctx)
	if err != nil {
		return err
	}
	start := yesterday
	if ok {
		start = last.Day.AddDays(1)
	}
	if oldest := yesterday.AddDays(-(rootBackfillDays - 1)); start.Before(oldest) {
		start = oldest
	}
	for day := start; !day.After(yesterday); day = day.AddDays(1) {
		root, err := s.makeRoot(ctx, day, last, ok)
		if err != nil {
			return fmt.Errorf("audit: daily root %s: %w", day, err)
		}
		last, ok = root, true
	}
	return nil
}

// makeRoot signs, archives and stores the root of day: the newest row
// chained before the day ended (or the previous root when nothing newer
// exists, e.g. after a purge).
func (s *service) makeRoot(ctx context.Context, day civil.Date, prev DailyRoot, hasPrev bool) (DailyRoot, error) {
	head, err := s.d.Store.HeadAt(ctx, day.AddDays(1).In(clock.Istanbul()))
	if err != nil {
		return DailyRoot{}, err
	}
	if hasPrev && prev.LastChainSeq > head.Seq {
		head = Link{Seq: prev.LastChainSeq, Hash: prev.RootHash}
	}
	sig, keyID, err := s.d.Signer.Sign(ctx, rootMessage(day, head.Seq, head.Hash))
	if err != nil {
		return DailyRoot{}, fmt.Errorf("sign: %w", err)
	}
	doc, err := json.Marshal(rootDoc{Version: 1, Day: day.String(), LastChainSeq: head.Seq,
		RootHash: hex.EncodeToString(head.Hash), Signature: base64.StdEncoding.EncodeToString(sig), KeyID: keyID})
	if err != nil {
		return DailyRoot{}, fmt.Errorf("encode: %w", err)
	}
	// External I/O stays outside any transaction (05 §6.2).
	ref, err := s.d.Archive.Put(ctx, archiveKey(day), doc)
	if err != nil {
		return DailyRoot{}, fmt.Errorf("archive: %w", err)
	}
	root := DailyRoot{Day: day, LastChainSeq: head.Seq, RootHash: head.Hash, Signature: sig, KeyID: keyID, ExternalRef: ref}
	switch err := s.d.Store.InsertRoot(ctx, root); {
	case errors.Is(err, ErrRootExists):
		stored, _, err := s.d.Store.RootBefore(ctx, day.AddDays(1))
		return stored, err
	case err != nil:
		return DailyRoot{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "audit daily root stored",
		slog.String("day", day.String()), slog.Int64("last_chain_seq", head.Seq), slog.String("key_id", keyID))
	return root, nil
}

// verifyRecent is audit.verify_chain: it verifies the last days, checks
// that yesterday has a root and that no row waits too long for the chain,
// raises the alarm on any finding and records the check in the log.
func (s *service) verifyRecent(ctx context.Context) error {
	now := s.now()
	today := civil.Today(now)
	from := today.AddDays(-verifyRecentDays)
	res, err := s.verifyRange(ctx, from, today)
	if err != nil {
		s.alarm(ctx, Alarm{Kind: AlarmChainBroken, Detail: "verification failed: " + err.Error()})
		return err
	}
	if !res.OK {
		s.alarm(ctx, Alarm{Kind: AlarmChainBroken, FirstBadChainSeq: res.FirstBadChainSeq, Detail: res.Problem})
	}
	yesterday := today.AddDays(-1)
	root, ok, err := s.d.Store.RootBefore(ctx, today)
	if err != nil {
		return err
	}
	if !ok || root.Day != yesterday {
		s.alarm(ctx, Alarm{Kind: AlarmRootFailed, Detail: "no daily root for " + yesterday.String()})
	}
	if oldest, ok, err := s.d.Store.OldestUnchained(ctx); err != nil {
		return err
	} else if ok && now.Sub(oldest) > chainLagAlarm {
		s.alarm(ctx, Alarm{Kind: AlarmChainLag, Detail: "unchained rows older than " + chainLagAlarm.String()})
	}
	outcome := Success
	if !res.OK {
		outcome = Failure
	}
	return s.Log(ctx, Event{Action: string(authz.AuditChainVerify), Outcome: outcome,
		Metadata: verifyMeta(from, today, res)}.By(authz.SystemActor(JobVerifyChain)))
}

func verifyMeta(from, to civil.Date, res VerifyResult) map[string]any {
	m := map[string]any{"from_day": from.String(), "to_day": to.String(), "checked_rows": res.CheckedRows, "ok": res.OK}
	if res.FirstBadChainSeq != nil {
		m["first_bad_chain_seq"] = *res.FirstBadChainSeq
		m["problem"] = res.Problem
	}
	return m
}
