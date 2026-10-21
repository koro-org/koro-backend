package kvkk_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"koro/internal/authz"
	"koro/internal/kvkk"
	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
)

// fakeSweeper is a module's RetentionSweeper: rows older than the cut-off
// are "eligible"; a real (approved) sweep removes them, a dry run only
// counts them. It records every call.
type fakeSweeper struct {
	codes []string
	table string
	fail  error

	mu       sync.Mutex
	eligible map[string]int
	calls    []privacy.RetentionPolicy
	inTx     []bool
	order    *callLog
}

func (f *fakeSweeper) PolicyCodes() []string { return f.codes }

func (f *fakeSweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, _ time.Time) (privacy.SweepResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p)
	f.inTx = append(f.inTx, db.InTx(ctx))
	if f.order != nil {
		f.order.add(p.Code)
	}
	n := f.eligible[p.Code]
	if f.fail != nil {
		return privacy.SweepResult{PolicyCode: p.Code, Table: f.table, Method: p.Method, Count: 1}, f.fail
	}
	if p.LegalApproved {
		f.eligible[p.Code] = 0 // destroyed
	}
	return privacy.SweepResult{PolicyCode: p.Code, Table: f.table, Method: p.Method, Count: n}, nil
}

func (e *env) sweep(t *testing.T) {
	t.Helper()
	if err := e.job(t, kvkk.RetentionSweepJobName).Handle(workerCtx(t)); err != nil {
		t.Fatalf("retention sweep: %v", err)
	}
}

func (e *env) logs(t *testing.T, code string) []kvkk.DestructionLog {
	t.Helper()
	ls, err := e.records.DestructionLogs(t.Context(), kvkk.DestructionFilter{PolicyCode: code}, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return ls
}

func (e *env) approve(t *testing.T, code string) {
	t.Helper()
	p, err := e.records.Policy(t.Context(), code)
	if err != nil {
		t.Fatal(err)
	}
	p.LegalApproved, p.ApprovedRef = true, "Hukuk görüşü 2026/21"
	e.records.PutPolicy(p)
}

// TestRetentionSweep_NotLegallyApproved_DryRunOnly (T-35): a policy the
// lawyer has not approved is swept as a dry run only: the sweeper is told
// (LegalApproved = false), it runs inside a transaction that kvkk rolls
// back, nothing is destroyed and the destruction record says so with the
// eligible count. After the approval the same policy destroys.
func TestRetentionSweep_NotLegallyApproved_DryRunOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sw := &fakeSweeper{codes: []string{"notification_delivery"}, table: "notification_deliveries",
		eligible: map[string]int{"notification_delivery": 42}}
	e.sweepers = []privacy.RetentionSweeper{sw}

	e.sweep(t)
	if len(sw.calls) != 1 || sw.calls[0].LegalApproved || !sw.inTx[0] {
		t.Fatalf("dry run calls = %+v in tx %v; want one unapproved call inside a transaction", sw.calls, sw.inTx)
	}
	if want := 12 * 30 * 24 * time.Hour; sw.calls[0].Retention < want || sw.calls[0].Method != privacy.MethodHardDelete {
		t.Fatalf("policy handed to the sweeper = %+v", sw.calls[0])
	}
	if sw.eligible["notification_delivery"] != 42 {
		t.Fatal("a dry run destroyed rows")
	}
	logs := e.logs(t, "notification_delivery")
	if len(logs) != 1 || logs[0].RecordCount != 0 || logs[0].Details["dry_run"] != true || logs[0].Details["eligible"] != 42 {
		t.Fatalf("dry run record = %+v", logs)
	}
	if got := e.audit.of(string(authz.KvkkRetentionSweep)); len(got) != 0 {
		t.Fatalf("a dry run audited a destruction: %+v", got)
	}

	e.approve(t, "notification_delivery")
	e.clk.Advance(24 * time.Hour)
	e.sweep(t)
	if last := sw.calls[len(sw.calls)-1]; !last.LegalApproved || sw.inTx[len(sw.inTx)-1] {
		t.Fatalf("approved run = %+v, in tx %v; want approved, outside kvkk's transaction (the sweeper batches)", last, sw.inTx)
	}
	logs = e.logs(t, "notification_delivery")
	if len(logs) != 2 || logs[0].RecordCount != 42 || logs[0].Details["dry_run"] != false {
		t.Fatalf("approved run record = %+v", logs[0])
	}
	if got := e.audit.of(string(authz.KvkkRetentionSweep)); len(got) != 1 {
		t.Fatalf("approved destruction audited %d times, want 1", len(got))
	}

	// A disabled policy is skipped: no call, no row.
	p, _ := e.records.Policy(t.Context(), "notification_delivery")
	p.Enabled = false
	e.records.PutPolicy(p)
	calls := len(sw.calls)
	e.sweep(t)
	if len(sw.calls) != calls || len(e.logs(t, "notification_delivery")) != 2 {
		t.Fatal("a disabled policy was swept")
	}
}

// TestRetentionSweep_WritesDestructionLogPerResult (01 §7, 02 §16.3): one
// destruction_logs row per policy and run, with the run ID, the module,
// the table and the cut-off; a failing sweeper is recorded (partial count,
// scrubbed error) and the run goes on; dependents run before the policies
// they reference (T-152).
func TestRetentionSweep_WritesDestructionLogPerResult(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	order := &callLog{}
	feed := &fakeSweeper{codes: []string{"post_removed"}, table: "posts", eligible: map[string]int{"post_removed": 3}, order: order}
	outreach := &fakeSweeper{codes: []string{"outreach_dispatch"}, table: "outreach_dispatches",
		eligible: map[string]int{"outreach_dispatch": 5}, order: order}
	broken := &fakeSweeper{codes: []string{"destruction_log"}, table: "destruction_logs", order: order,
		fail: errors.New("purge failed for kisi@example.test")}
	unknown := &fakeSweeper{codes: []string{"no_such_policy"}, eligible: map[string]int{}}
	e.sweepers = []privacy.RetentionSweeper{feed, broken, unknown, outreach}
	for _, c := range []string{"post_removed", "outreach_dispatch", "destruction_log"} {
		e.approve(t, c)
	}
	e.sweep(t)

	if got := order.all(); !slices.Equal(got, []string{"outreach_dispatch", "post_removed", "destruction_log"}) {
		t.Fatalf("run order = %v; dispatches before removed posts, the destruction log last", got)
	}
	all, err := e.records.DestructionLogs(t.Context(), kvkk.DestructionFilter{}, nil, 100)
	if err != nil || len(all) != 3 {
		t.Fatalf("destruction rows = %d, %v; want 3 (one per policy, none for the unknown code)", len(all), err)
	}
	run := all[0].RunID
	now := e.clk.Now()
	for _, l := range all {
		if l.RunID != run {
			t.Fatalf("rows of one run carry different run IDs: %+v", all)
		}
		switch l.PolicyCode {
		case "post_removed":
			if l.RecordCount != 3 || l.Module != "feed" || l.Target != "posts" || !l.PeriodCutoff.Equal(now.AddDate(-1, 0, 0)) {
				t.Errorf("post_removed row = %+v", l)
			}
		case "outreach_dispatch":
			if l.RecordCount != 5 || l.Module != "outreach" {
				t.Errorf("outreach_dispatch row = %+v", l)
			}
		case "destruction_log":
			msg, _ := l.Details["error"].(string)
			if l.RecordCount != 1 || msg != "sweep_failed" {
				t.Errorf("failed sweep row = %+v; want the partial count and a scrubbed error", l)
			}
		}
	}
	// Re-running finds nothing new: the rows say 0.
	e.sweep(t)
	if l := e.logs(t, "post_removed"); len(l) != 2 || l[0].RecordCount != 0 {
		t.Fatalf("second run = %+v", l)
	}
}

// TestSweepOrder (T-152, H-41): the referenced policies run after the
// referencing ones; the destruction log last.
func TestSweepOrder(t *testing.T) {
	t.Parallel()
	got := kvkk.SweepOrder([]string{"destruction_log", "post_removed", "audit_roots", "contact_request_closed",
		"audit_events", "outreach_dispatch", "talent_ledger", "talent_runs", "talent_counters"})
	want := []string{"audit_events", "contact_request_closed", "outreach_dispatch", "talent_counters", "talent_runs",
		"audit_roots", "post_removed", "talent_ledger", "destruction_log"}
	if !slices.Equal(got, want) {
		t.Fatalf("SweepOrder = %v, want %v", got, want)
	}
}

// TestUpdateRetentionPolicy_RequiresReauth_ETag (03 §6.2, 04 §1.13): the
// update is R (no or wrong proof: 403, audited as denied), optimistic
// (428 / 412), and a changed period withdraws the approval unless the same
// request approves it with its reference. Only dpo may.
func TestUpdateRetentionPolicy_RequiresReauth_ETag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	dpo := actor(t, "dpo")
	cur, _ := e.records.Policy(ctx, "notification_delivery")
	etag := kvkk.ETagOf(cur.UpdatedAt)
	p13 := kvkk.Period{Years: 1, Months: 1}
	patch := kvkk.PolicyPatch{Period: &p13}

	if _, err := e.svc.UpdateRetentionPolicy(ctx, dpo, "notification_delivery", etag, patch, kvkk.ReauthProof{}); !errors.Is(err, errBadProof) {
		t.Fatalf("without proof = %v, want the reauth failure", err)
	}
	if got := e.audit.of(string(authz.KvkkRetentionPolicyUpdate)); len(got) != 1 || got[0].Reason != "reauth_failed" {
		t.Fatalf("failed reauth audit = %+v", got)
	}
	if _, err := e.svc.UpdateRetentionPolicy(ctx, dpo, "notification_delivery", "", patch, proof); !errors.Is(err, problem.ErrPreconditionRequired) {
		t.Fatalf("without If-Match = %v", err)
	}
	if _, err := e.svc.UpdateRetentionPolicy(ctx, dpo, "notification_delivery", `"stale"`, patch, proof); !errors.Is(err, problem.ErrPreconditionFailed) {
		t.Fatalf("stale If-Match = %v", err)
	}
	if _, err := e.svc.UpdateRetentionPolicy(ctx, actor(t, "platform_admin"), "notification_delivery", etag, patch, proof); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("platform_admin = %v, want forbidden", err)
	}
	// Approve with a reference: the period and the approval move together.
	yes, ref := true, "Hukuk görüşü 2026/21"
	if _, err := e.svc.UpdateRetentionPolicy(ctx, dpo, "notification_delivery", etag,
		kvkk.PolicyPatch{LegalApproved: &yes}, proof); !errors.Is(err, kvkk.ErrInvalidRequest) {
		t.Fatalf("approval without a reference = %v", err)
	}
	got, err := e.svc.UpdateRetentionPolicy(ctx, dpo, "notification_delivery", etag,
		kvkk.PolicyPatch{Period: &p13, LegalApproved: &yes, ApprovedRef: &ref}, proof)
	if err != nil || !got.LegalApproved || got.ApprovedRef != ref || got.Period != p13 || got.UpdatedBy == nil || *got.UpdatedBy != dpo.UserID {
		t.Fatalf("approved update = %+v, %v", got, err)
	}
	if kvkk.ETagOf(got.UpdatedAt) == etag {
		t.Fatal("the ETag did not change")
	}
	// A later period change alone withdraws the approval.
	p2 := kvkk.Period{Years: 2}
	got, err = e.svc.UpdateRetentionPolicy(ctx, dpo, "notification_delivery", kvkk.ETagOf(got.UpdatedAt), kvkk.PolicyPatch{Period: &p2}, proof)
	if err != nil || got.LegalApproved || got.ApprovedRef != "" || got.Period != p2 {
		t.Fatalf("period change = %+v, %v; the approval must be withdrawn", got, err)
	}
	ok := e.audit.of(string(authz.KvkkRetentionPolicyUpdate))
	if last := ok[len(ok)-1]; last.Metadata["reauth"] != "password" || last.Metadata["period_to"] != "P2Y" {
		t.Fatalf("audit of the update = %+v", last.Metadata)
	}
	// T-83: the export files never live longer than 7 days.
	exp, _ := e.records.Policy(ctx, kvkk.ExportPolicyCode)
	p30 := kvkk.Period{Days: 30}
	if _, err := e.svc.UpdateRetentionPolicy(ctx, dpo, kvkk.ExportPolicyCode, kvkk.ETagOf(exp.UpdatedAt),
		kvkk.PolicyPatch{Period: &p30}, proof); !errors.Is(err, kvkk.ErrInvalidRequest) {
		t.Fatalf("30-day export = %v", err)
	}
	if _, err := e.svc.UpdateRetentionPolicy(ctx, dpo, "no_such_policy", etag, patch, proof); !errors.Is(err, kvkk.ErrNotFound) {
		t.Fatalf("unknown policy = %v", err)
	}
}

// TestRetentionPolicies_ListShowsSweepers: the DPO sees which policies a
// sweeper runs.
func TestRetentionPolicies_ListShowsSweepers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sweepers = []privacy.RetentionSweeper{&fakeSweeper{codes: []string{"post_removed"}}}
	ps, err := e.svc.ListRetentionPolicies(t.Context(), actor(t, "platform_admin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.SweeperRegistered != (p.Code == "post_removed") {
			t.Errorf("%s sweeper_registered = %v", p.Code, p.SweeperRegistered)
		}
	}
	if _, err := e.svc.ListRetentionPolicies(t.Context(), actor(t, "student")); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("student = %v", err)
	}
}

// TestExportRetention_PolicyShortensNeverLengthens (T-192): the export
// purge follows dsr_export_file when it is shorter, never beyond 7 days.
func TestExportRetention_PolicyShortensNeverLengthens(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p, _ := e.records.Policy(t.Context(), kvkk.ExportPolicyCode)
	p.Period = kvkk.Period{Days: 2}
	e.records.PutPolicy(p)
	d := e.completeExport(t)
	e.clk.Advance(3 * 24 * time.Hour)
	if _, err := e.svc.DownloadMine(t.Context(), actor(t, "student"), d.ID); !errors.Is(err, kvkk.ErrExportExpired) {
		t.Fatalf("download after the 2-day policy = %v, want expired", err)
	}
}

func TestParsePeriod(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"P3Y": "P3Y", "P12M": "P1Y", "P1Y6M": "P1Y6M", "P30D": "P30D", "PT24H": "PT24H",
		"P0D": "P0D", "P1Y2M3DT4H": "P1Y2M3DT4H"} {
		p, err := kvkk.ParsePeriod(in)
		if err != nil || p.String() != want {
			t.Errorf("ParsePeriod(%q) = %v, %v; want %s", in, p, err, want)
		}
	}
	for _, bad := range []string{"", "P", "3Y", "PT", "P1D2Y", "P1W", "P-1D", "P1Y1Y", "P101Y", "PT1M"} {
		if _, err := kvkk.ParsePeriod(bad); err == nil {
			t.Errorf("ParsePeriod(%q) accepted", bad)
		}
	}
	// Calendar arithmetic: ten years end on the same calendar day.
	at := time.Date(2036, 2, 29, 12, 0, 0, 0, time.UTC)
	if got := (kvkk.Period{Years: 10}).Before(at); !got.Equal(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("10 years before %s = %s", at, got)
	}
}

// completeExport has the student file an access request, the DPO answer it
// and the worker build the export; it returns the request.
func (e *env) completeExport(t *testing.T) kvkk.DSR {
	t.Helper()
	d, err := e.svc.CreateMine(t.Context(), actor(t, "student"), kvkk.MyRequest{Type: kvkk.TypeAccess})
	if err != nil {
		t.Fatal(err)
	}
	e.completeAs(t, d.ID)
	if _, err := e.deliver(t, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DownloadMine(t.Context(), actor(t, "student"), d.ID); err != nil {
		t.Fatalf("fresh export: %v", err)
	}
	return d
}
