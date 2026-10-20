package kvkk

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/page"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// Retention: policies (02 §16.2), the nightly sweep (01 §7
// kvkk.retention_sweep) and the destruction record (02 §16.3).

const (
	// RetentionSweepJobName runs every registered RetentionSweeper (daily
	// 02:30, low).
	RetentionSweepJobName = "kvkk.retention_sweep"

	sweepCron   = "30 2 * * *"
	sweepUnique = 23 * time.Hour
	// sweepTimeout bounds one night's run (sweepers batch ≤ 1000 rows/tx).
	sweepTimeout = 2 * time.Hour

	maxApprovedRef = 200
	resourcePolicy = "retention_policy"
)

// PolicyView is a policy with whether a sweeper is registered for it.
type PolicyView struct {
	Policy
	SweeperRegistered bool
}

// PolicyPatch is the body of adminUpdateRetentionPolicy; nil fields stay.
type PolicyPatch struct {
	Period        *Period
	Method        *string
	Enabled       *bool
	LegalApproved *bool
	ApprovedRef   *string
}

// DestructionPage is one page of the destruction record.
type DestructionPage struct {
	Items []DestructionLog
	Next  *After
}

// ETagOf is the strong ETag of a record (04 §1.13): base32 of updated_at
// in Unix nanoseconds, quoted.
func ETagOf(updatedAt time.Time) string { return ETag(DSR{UpdatedAt: updatedAt}) }

// ifMatches compares an If-Match header with the current ETag (a weak
// prefix is ignored, as on the request routes).
func ifMatches(ifMatch string, updatedAt time.Time) bool {
	return ETagOf(updatedAt) == strings.TrimPrefix(ifMatch, "W/")
}

// sweeperCodes maps every policy code to its registered sweeper. A code
// claimed twice keeps the first sweeper; the others are reported.
func (s *service) sweeperCodes() (map[string]privacy.RetentionSweeper, []string) {
	out, dup := map[string]privacy.RetentionSweeper{}, []string(nil)
	for _, sw := range s.d.Sweepers() {
		if sw == nil {
			continue
		}
		for _, code := range sw.PolicyCodes() {
			if _, taken := out[code]; taken {
				dup = append(dup, code)
				continue
			}
			out[code] = sw
		}
	}
	return out, dup
}

// ListRetentionPolicies implements Service (adminListRetentionPolicies).
func (s *service) ListRetentionPolicies(ctx context.Context, a authz.Actor) ([]PolicyView, error) {
	if err := authz.Can(ctx, a, authz.KvkkRetentionPolicyList, adminSurface); err != nil {
		return nil, err
	}
	ps, err := s.d.Records.Policies(ctx)
	if err != nil {
		return nil, err
	}
	sweepers, _ := s.sweeperCodes()
	out := make([]PolicyView, len(ps))
	for i, p := range ps {
		_, ok := sweepers[p.Code]
		out[i] = PolicyView{Policy: p, SweeperRegistered: ok}
	}
	return out, nil
}

// policyEvent is the audit row of a policy change (no personal data).
func policyEvent(a authz.Actor, act authz.Action, meta map[string]any) audit.Event {
	return audit.Event{Action: string(act), Outcome: audit.Success, ResourceType: resourcePolicy, Metadata: meta}.By(a)
}

// UpdateRetentionPolicy implements Service (adminUpdateRetentionPolicy, R).
//
// Rules (T-35): an approval is of a period and a method; changing either
// withdraws the approval unless the same request approves the new values
// with their reference. legal_approved = true needs approved_ref
// (000089 CHECK). The export files never live longer than 7 days (T-83,
// 06 H-48).
func (s *service) UpdateRetentionPolicy(ctx context.Context, a authz.Actor, code, ifMatch string, p PolicyPatch, proof ReauthProof) (PolicyView, error) {
	if err := authz.Can(ctx, a, authz.KvkkRetentionPolicyUpdate, adminSurface); err != nil {
		return PolicyView{}, err
	}
	kind, err := s.reauth(ctx, a, authz.KvkkRetentionPolicyUpdate, resourcePolicy, nil, proof)
	if err != nil {
		return PolicyView{}, err
	}
	if p == (PolicyPatch{}) {
		return PolicyView{}, invalid("body", "nothing to update")
	}
	if ifMatch == "" {
		return PolicyView{}, problem.ErrPreconditionRequired
	}
	if p.Method != nil && !slices.Contains(policyMethods, *p.Method) {
		return PolicyView{}, invalid("body.method", "must be hard_delete, anonymize, null_columns or file_delete")
	}
	var ref string
	if p.ApprovedRef != nil {
		if ref, err = cleanText(*p.ApprovedRef, "body.approved_ref", maxApprovedRef, false); err != nil {
			return PolicyView{}, err
		}
	}
	var out Policy
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Records.LockPolicy(ctx, code)
		if err != nil {
			return err
		}
		if !ifMatches(ifMatch, cur.UpdatedAt) {
			return problem.ErrPreconditionFailed
		}
		next := cur
		var changed []string
		if p.Period != nil && *p.Period != cur.Period {
			next.Period = *p.Period
			changed = append(changed, "period")
		}
		if p.Method != nil && *p.Method != cur.Method {
			next.Method = *p.Method
			changed = append(changed, "method")
		}
		if code == ExportPolicyCode && next.Period.Duration(s.now()) > MaxExportRetention {
			return invalid("body.period", "the export files live at most 7 days (T-83)")
		}
		if p.Enabled != nil && *p.Enabled != cur.Enabled {
			next.Enabled = *p.Enabled
			changed = append(changed, "enabled")
		}
		valuesChanged := next.Period != cur.Period || next.Method != cur.Method
		switch {
		case p.LegalApproved != nil && *p.LegalApproved:
			if ref == "" {
				if valuesChanged || cur.ApprovedRef == "" {
					return invalid("body.approved_ref", "an approval names its reference (document / meeting)")
				}
				ref = cur.ApprovedRef
			}
			next.LegalApproved, next.ApprovedRef = true, ref
		case p.LegalApproved != nil || valuesChanged:
			// Withdrawn, or the approval was of the old period / method.
			next.LegalApproved, next.ApprovedRef = false, ""
		case p.ApprovedRef != nil:
			if !cur.LegalApproved || ref == "" {
				return invalid("body.approved_ref", "a reference is corrected on an approved policy only")
			}
			next.ApprovedRef = ref
		}
		if next.LegalApproved != cur.LegalApproved {
			changed = append(changed, "legal_approved")
		}
		if next.ApprovedRef != cur.ApprovedRef {
			changed = append(changed, "approved_ref")
		}
		if len(changed) == 0 {
			out = cur
			return nil
		}
		me := a.UserID
		next.UpdatedBy = &me
		if err := s.d.Records.UpdatePolicy(ctx, next); err != nil {
			return err
		}
		meta := map[string]any{"code": code, "changed": strings.Join(changed, ","), audit.MetaReauth: kind,
			"period_from": cur.Period.String(), "period_to": next.Period.String(), "method": next.Method,
			"enabled": next.Enabled, "legal_approved": next.LegalApproved}
		if err := s.d.Audit.Log(ctx, policyEvent(a, authz.KvkkRetentionPolicyUpdate, meta)); err != nil {
			return err
		}
		out, err = s.d.Records.Policy(ctx, code)
		return err
	})
	if err != nil {
		return PolicyView{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "retention policy updated", slog.String("code", code),
		slog.Bool("legal_approved", out.LegalApproved), slog.Bool("enabled", out.Enabled))
	sweepers, _ := s.sweeperCodes()
	_, ok := sweepers[code]
	return PolicyView{Policy: out, SweeperRegistered: ok}, nil
}

// ListDestructionLogs implements Service (adminListDestructionLogs).
func (s *service) ListDestructionLogs(ctx context.Context, a authz.Actor, f DestructionFilter, after *After, limit int) (DestructionPage, error) {
	if err := authz.Can(ctx, a, authz.KvkkDestructionLogList, adminSurface); err != nil {
		return DestructionPage{}, err
	}
	limit = page.ClampLimit(limit)
	rows, err := s.d.Records.DestructionLogs(ctx, f, after, limit+1)
	if err != nil {
		return DestructionPage{}, err
	}
	items, more := page.Cut(rows, limit)
	p := DestructionPage{Items: items}
	if more {
		last := items[len(items)-1]
		p.Next = &After{At: last.CreatedAt, ID: last.ID}
	}
	return p, nil
}

// exportRetention is how long an export file lives: the dsr_export_file
// policy when it is enabled, never longer than KVKK_EXPORT_RETENTION
// (≤ 7 days, T-83/T-192). A store error falls back to the configuration.
func (s *service) exportRetention(ctx context.Context) time.Duration {
	p, err := s.d.Records.Policy(ctx, ExportPolicyCode)
	if err != nil || !p.Enabled {
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "export retention policy unreadable; using KVKK_EXPORT_RETENTION",
				slog.String("err", obs.Scrub(err.Error())))
		}
		return s.d.ExportRetention
	}
	if d := p.Period.Duration(s.now()); d > 0 && d < s.d.ExportRetention {
		return d
	}
	return s.d.ExportRetention
}

// ---------------------------------------------------------------- sweep

// sweepRank orders the policies of a run (T-152, H-41): a policy whose rows
// are referenced by another policy's rows runs after it, so the referencing
// rows are gone first. Rank 0 (default) runs first; ties by code.
//
//	post_removed            after outreach_dispatch, contact_request_closed
//	                        (their post_id FKs; feed skips a removed post that
//	                        is still referenced and deletes it in a later run)
//	edu_verification_history after edu_document (the document file)
//	audit_roots             after audit_events (verification starts at a root)
//	dsr_record              after dsr_export_file
//	destruction_log         last (tonight's rows are written by this run)
var sweepRank = map[string]int{
	"post_removed":             1,
	"edu_verification_history": 1,
	"audit_roots":              1,
	"dsr_record":               1,
	"destruction_log":          2,
}

// SweepOrder sorts policy codes into the run order.
func SweepOrder(codes []string) []string {
	out := slices.Clone(codes)
	slices.SortFunc(out, func(a, b string) int {
		return cmp.Or(cmp.Compare(sweepRank[a], sweepRank[b]), strings.Compare(a, b))
	})
	return out
}

func (s *service) retentionSweepJob() queue.Job {
	return queue.Job{Name: RetentionSweepJobName, Cron: sweepCron, Queue: queue.Low, Unique: sweepUnique, Timeout: sweepTimeout,
		Handle: func(ctx context.Context) error { _, err := s.sweep(ctx); return err }}
}

// SweepReport summarises one run.
type SweepReport struct {
	RunID     uuid.UUID
	Ran       int // policies swept (dry runs included)
	DryRuns   int
	Destroyed int
	Failed    int
	Skipped   int // disabled policies
}

// errDryRun rolls a dry run back whatever the sweeper did.
var errDryRun = errors.New("kvkk: dry run (rolled back)")

// sweep is kvkk.retention_sweep: every registered sweeper, policy by
// policy in SweepOrder, one destruction_logs row per policy run.
//
//   - disabled policy (enabled = false): skipped, no row;
//   - not legally approved (the seed state): dry run. The sweeper gets
//     LegalApproved = false (count only) and runs inside a transaction that
//     is always rolled back, so even a faulty sweeper destroys nothing; the
//     row says dry_run with the eligible count and record_count 0;
//   - approved: the sweeper deletes in its own batches (≤ 1000 rows per
//     transaction, idempotent: an interrupted run continues next time) and
//     the row records the count. A failure is recorded too (the count of
//     what was destroyed before it, the error class) and the run goes on
//     with the next policy.
//
// Only a failed destruction_logs write fails the job (asynq retries it):
// a destruction must not go unrecorded.
func (s *service) sweep(ctx context.Context) (SweepReport, error) {
	sys := authz.SystemActor(RetentionSweepJobName)
	if err := authz.Can(ctx, sys, authz.KvkkRetentionSweep, authz.Resource{}); err != nil {
		return SweepReport{}, queue.Permanent(err)
	}
	rep := SweepReport{RunID: uuid.NewV7()}
	sweepers, dup := s.sweeperCodes()
	for _, code := range dup {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "retention policy claimed by two sweepers; the first one runs",
			slog.String("code", code), slog.Bool("alarm", true))
	}
	policies, err := s.d.Records.Policies(ctx)
	if err != nil {
		return rep, err
	}
	byCode := make(map[string]Policy, len(policies))
	for _, p := range policies {
		byCode[p.Code] = p
	}
	now := s.now()
	for _, code := range SweepOrder(slicesKeys(sweepers)) {
		p, ok := byCode[code]
		switch {
		case !ok:
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "sweeper names an unknown retention policy", slog.String("code", code))
			continue
		case !p.Enabled:
			rep.Skipped++
			continue
		}
		destroyed, failed, err := s.sweepPolicy(ctx, sweepers[code], p, rep.RunID, now)
		if err != nil {
			return rep, err
		}
		rep.Ran++
		rep.Destroyed += destroyed
		if !p.LegalApproved {
			rep.DryRuns++
		}
		if failed {
			rep.Failed++
		}
	}
	for code, p := range byCode {
		if _, ok := sweepers[code]; !ok && p.Enabled && code != ErasurePolicyCode && code != ExportPolicyCode {
			s.d.Log.LogAttrs(ctx, slog.LevelDebug, "retention policy without a registered sweeper", slog.String("code", code))
		}
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "retention sweep finished", slog.String("run_id", rep.RunID.String()),
		slog.Int("policies", rep.Ran), slog.Int("dry_runs", rep.DryRuns), slog.Int("destroyed", rep.Destroyed),
		slog.Int("failed", rep.Failed), slog.Int("skipped", rep.Skipped))
	return rep, nil
}

func slicesKeys(m map[string]privacy.RetentionSweeper) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// maxErrDetail bounds the error text kept in a destruction row.
const maxErrDetail = 300

// sweepPolicy runs one policy and records it.
func (s *service) sweepPolicy(ctx context.Context, sw privacy.RetentionSweeper, p Policy, runID uuid.UUID, now time.Time) (destroyed int, failed bool, err error) {
	rp := p.RetentionPolicy(now)
	dry := !rp.LegalApproved
	started := s.now()
	var res privacy.SweepResult
	var serr error
	if dry {
		terr := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			res, serr = sw.Sweep(ctx, rp, now)
			return errDryRun
		})
		if !errors.Is(terr, errDryRun) && serr == nil {
			serr = terr
		}
	} else {
		res, serr = sw.Sweep(ctx, rp, now)
	}
	details := map[string]any{"dry_run": dry}
	count := max(res.Count, 0)
	if dry {
		details["eligible"] = count
		count = 0
	}
	if serr != nil {
		failed = true
		// The record keeps the error class only (02 §16.3: no personal
		// data); the scrubbed message goes to the log.
		details["error"] = "sweep_failed"
		if st := db.SQLState(serr); st != "" {
			details["error"] = "sqlstate_" + st
		}
		msg := obs.Scrub(serr.Error())
		if len([]rune(msg)) > maxErrDetail {
			msg = string([]rune(msg)[:maxErrDetail])
		}
		s.d.Log.LogAttrs(ctx, slog.LevelError, "retention sweep failed", slog.String("code", p.Code),
			slog.String("run_id", runID.String()), slog.Bool("dry_run", dry), slog.Int("destroyed", count),
			slog.String("err", msg), slog.Bool("alarm", true))
	}
	l := DestructionLog{ID: uuid.NewV7(), PolicyCode: p.Code, RunID: runID, Module: p.OwnerModule,
		Target: cmp.Or(res.Table, p.Target), Method: p.Method, RecordCount: count, PeriodCutoff: p.Period.Before(now),
		StartedAt: started, FinishedAt: s.now(), Details: details}
	if res.Method != "" && slices.Contains(policyMethods, res.Method) {
		l.Method = res.Method
	}
	if err := s.d.Records.InsertDestructionLog(ctx, l); err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "destruction record not written", slog.String("code", p.Code),
			slog.String("run_id", runID.String()), slog.Int("destroyed", count), slog.Bool("alarm", true))
		return count, failed, fmt.Errorf("kvkk: destruction log of %s: %w", p.Code, err)
	}
	if count > 0 {
		if err := s.d.Audit.Log(ctx, policyEvent(authz.SystemActor(RetentionSweepJobName), authz.KvkkRetentionSweep,
			map[string]any{"code": p.Code, "run_id": runID.String(), "count": count, "method": l.Method})); err != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "retention sweep not audited", slog.String("code", p.Code),
				slog.String("err", obs.Scrub(err.Error())))
		}
	}
	return count, failed, nil
}
