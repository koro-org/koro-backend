package kvkk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// IdentityProvider is the name of identity's privacy provider: it erases
// last (01 §6.3) because its tombstone keeps the foreign keys of retained
// rows valid and its Erase deletes every session.
const IdentityProvider = "identity"

// erasureJob is the system actor of the erasure run.
const erasureJob = "kvkk.erase_account"

// eraseTimeout bounds one erasure run (every provider twice).
const eraseTimeout = 10 * time.Minute

// exportTimeout bounds one export build.
const exportTimeout = 10 * time.Minute

// erasureReportHeader starts the report appended to response_summary when
// the run is done; it also marks the run as finished (idempotency).
const erasureReportHeader = "— Hesap silme raporu —"

// handleErasureApproved is the dsr.erasure_approved subscription
// (kvkk.erase_account, 01 §6.3 "Hesap silme (worker)").
func (s *service) handleErasureApproved(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.DSRErasureApproved](e)
	if err != nil {
		return queue.Permanent(err)
	}
	d, err := s.d.Store.Get(ctx, p.DSRID)
	switch {
	case errors.Is(err, ErrNotFound):
		return queue.Permanent(fmt.Errorf("kvkk: erasure of unknown request %s", p.DSRID))
	case err != nil:
		return err
	case d.Type != TypeErasure || d.UserID == nil || *d.UserID != p.UserID || d.Status != StatusCompleted:
		return queue.Permanent(fmt.Errorf("kvkk: request %s is not an approved erasure", d.ID))
	case strings.Contains(d.ResponseSummary, erasureReportHeader):
		return nil // done (at-least-once delivery)
	}
	return s.erase(ctx, d)
}

// erase runs the erasure of d's subject:
//
//  1. every provider but identity erases, each in its own transaction
//     (providers are idempotent: a failure returns the error and the task
//     is retried from the start);
//  2. the subject gets the completion e-mail while the address exists;
//  3. identity anonymises the account (tombstone) and deletes every
//     session and e-mail token: the subject is signed out everywhere;
//  4. every provider but identity runs once more, sweeping what the
//     subject wrote while step 1 ran (no session can write any more);
//  5. the report (deleted / anonymised / retained with the legal basis,
//     02 §0.6) is appended to the answer, audited, and dsr.completed is
//     written.
//
// The audit log is never touched: its rows keep the tombstone's UUID and
// the hash chain stays intact.
func (s *service) erase(ctx context.Context, d DSR) error {
	uid := *d.UserID
	ctx = privacy.WithSubject(ctx, privacy.Subject{UserID: uid, DSRID: d.ID})
	ps, err := s.providers()
	if err != nil {
		return err
	}
	var identity privacy.Provider
	others := make([]privacy.Provider, 0, len(ps))
	for _, p := range ps {
		if p.Name() == IdentityProvider {
			identity = p
			continue
		}
		others = append(others, p)
	}
	if identity == nil {
		// Without identity the account would survive: fail loudly, retry.
		s.d.Log.LogAttrs(ctx, slog.LevelError, "erasure blocked: identity privacy provider not registered",
			slog.String("dsr_id", d.ID.String()), slog.Bool("alarm", true))
		return errors.New("kvkk: identity privacy provider is not registered")
	}
	// 06 H-49: no session may write while the run goes (the approval
	// signed the subject out already; this covers a login since).
	if s.d.RevokeSessions != nil {
		if _, err := s.d.RevokeSessions(ctx, uid); err != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "sessions not revoked before the erasure run; retrying",
				slog.String("dsr_id", d.ID.String()))
			return fmt.Errorf("kvkk: revoke sessions: %w", err)
		}
	}
	started := s.now()
	results := map[string]privacy.EraseResult{}
	run := func(p privacy.Provider) error {
		r, err := p.Erase(ctx, uid)
		if err != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "erasure step failed, the run will be retried",
				slog.String("dsr_id", d.ID.String()), slog.String("provider", p.Name()))
			return fmt.Errorf("kvkk: erase %s: %w", p.Name(), err)
		}
		acc := results[p.Name()]
		acc.Module = p.Name()
		acc.Deleted += r.Deleted
		acc.Anonymized += r.Anonymized
		acc.Retained = max(acc.Retained, r.Retained)
		if r.RetainedReason != "" {
			acc.RetainedReason = r.RetainedReason
		}
		results[p.Name()] = acc
		return nil
	}
	for _, p := range others {
		if err := run(p); err != nil {
			return err
		}
	}
	if err := s.notifySubject(ctx, d, OutcomeErased); err != nil {
		return err
	}
	if err := run(identity); err != nil {
		return err
	}
	for _, p := range others {
		if err := run(p); err != nil {
			return err
		}
	}
	return s.finishErasure(ctx, d.ID, ps, results, started)
}

// revokeSessions signs the subject of an approved erasure out (after the
// approval committed). A failure is logged: the erasure run revokes again
// before it starts and identity deletes every session at its step.
func (s *service) revokeSessions(ctx context.Context, dsrID, userID uuid.UUID) {
	if s.d.RevokeSessions == nil {
		return
	}
	n, err := s.d.RevokeSessions(ctx, userID)
	if err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "sessions not revoked at the erasure approval; the run revokes them",
			slog.String("dsr_id", dsrID.String()))
		return
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "sessions revoked at the erasure approval", slog.String("dsr_id", dsrID.String()),
		slog.Int("sessions", n))
}

// erasureLogs are the destruction record rows of an erasure run (02 §16.3:
// policy dsr_erasure, one row per module; run_id = the request ID, so the
// record proves which request was carried out without naming the person).
func (s *service) erasureLogs(id uuid.UUID, ps []privacy.Provider, results map[string]privacy.EraseResult, started time.Time) []DestructionLog {
	now := s.now()
	out := make([]DestructionLog, 0, len(ps))
	for _, p := range ps {
		r := results[p.Name()]
		method := "hard_delete"
		if r.Anonymized > 0 && r.Deleted == 0 {
			method = "anonymize"
		}
		out = append(out, DestructionLog{ID: uuid.NewV7(), PolicyCode: ErasurePolicyCode, RunID: id, Module: p.Name(),
			Target: p.Name(), Method: method, RecordCount: r.Deleted + r.Anonymized, PeriodCutoff: now,
			StartedAt: started, FinishedAt: now, Details: map[string]any{"deleted": r.Deleted, "anonymized": r.Anonymized,
				"retained": r.Retained, "retained_reason": r.RetainedReason}})
	}
	return out
}

// finishErasure appends the report, audits the run and writes
// dsr.completed in one transaction.
func (s *service) finishErasure(ctx context.Context, id uuid.UUID, ps []privacy.Provider, results map[string]privacy.EraseResult, started time.Time) error {
	var deleted, anonymized, retained int
	var modules []string
	for _, p := range ps {
		r := results[p.Name()]
		deleted, anonymized, retained = deleted+r.Deleted, anonymized+r.Anonymized, retained+r.Retained
		modules = append(modules, p.Name())
	}
	report := erasureReport(ps, results)
	sys := authz.SystemActor(erasureJob)
	var done DSR
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.Lock(ctx, id)
		if err != nil {
			return err
		}
		if strings.Contains(cur.ResponseSummary, erasureReportHeader) {
			return nil
		}
		// The report (and its header, the "done" marker) always fits; the
		// DPO's answer gives way if both exceed the column.
		rep := cutRunes(report, maxSummary)
		room := max(maxSummary-len([]rune(rep))-2, 0)
		cur.ResponseSummary = strings.TrimSpace(cutRunes(strings.TrimSpace(cur.ResponseSummary), room) + "\n\n" + rep)
		if err := s.d.Store.Update(ctx, cur); err != nil {
			return err
		}
		for _, l := range s.erasureLogs(id, ps, results, started) {
			if err := s.d.Records.InsertDestructionLog(ctx, l); err != nil {
				return err
			}
		}
		if err := s.d.Audit.Log(ctx, event(sys, authz.KvkkDsrComplete, cur, map[string]any{
			"step": "erasure_done", "modules": strings.Join(modules, ","), "deleted": deleted,
			"anonymized": anonymized, "retained": retained})); err != nil {
			return err
		}
		done = cur
		return s.emit(ctx, domainevent.DSRCompleted{DSRID: cur.ID, UserID: *cur.UserID, Outcome: OutcomeErased}, cur.ID)
	})
	if err != nil {
		return err
	}
	if done.ID != uuid.Nil() {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "account erased", slog.String("dsr_id", id.String()),
			slog.Int("providers", len(ps)), slog.Int("deleted", deleted), slog.Int("anonymized", anonymized),
			slog.Int("retained", retained))
	}
	return nil
}

// erasureReport is the human-readable result appended to the answer
// (02 §0.6: retained records are listed with their legal basis).
func erasureReport(ps []privacy.Provider, results map[string]privacy.EraseResult) string {
	var b strings.Builder
	b.WriteString(erasureReportHeader)
	b.WriteString("\nHesabınız anonimleştirildi; oturumlarınız kapatıldı. Kişisel verileriniz silindi veya anonimleştirildi.")
	var kept []string
	for _, p := range ps {
		r := results[p.Name()]
		if r.Retained > 0 {
			kept = append(kept, fmt.Sprintf("- %s: %d kayıt — %s", p.Name(), r.Retained, r.RetainedReason))
		}
	}
	if len(kept) > 0 {
		b.WriteString("\nKanuni yükümlülük gereği saklanan kayıtlar (süre sonunda imha edilir):\n")
		b.WriteString(strings.Join(kept, "\n"))
	}
	return b.String()
}

// cutRunes shortens s to at most n runes.
func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
