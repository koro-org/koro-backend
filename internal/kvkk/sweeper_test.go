package kvkk_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/kvkk"
	"koro/internal/platform/privacy"
)

// approvePolicies marks the policies enabled and legally approved.
func approvePolicies(t *testing.T, e *env, codes ...string) {
	t.Helper()
	for _, code := range codes {
		p, err := e.records.Policy(t.Context(), code)
		if err != nil {
			t.Fatal(err)
		}
		p.Enabled, p.LegalApproved, p.ApprovedRef = true, true, "Hukuk görüşü 2026/21"
		e.records.PutPolicy(p)
	}
}

// TestSweeper_KvkkRecords (BE-057): kvkk's own sweeper. Unapproved, the
// nightly run only counts (dry run, nothing deleted); approved, a closed
// request older than 3 years, a breach closed more than 5 years ago and a
// destruction row older than 3 years go, and every run is recorded.
func TestSweeper_KvkkRecords(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	now := e.clk.Now()
	for _, p := range []kvkk.Policy{
		{Code: kvkk.PolicyDSRRecord, OwnerModule: "kvkk", Target: "data_subject_requests", Period: kvkk.Period{Years: 3}, Anchor: "closed_at"},
		{Code: kvkk.PolicyBreachRecord, OwnerModule: "kvkk", Target: "breach_incidents", Period: kvkk.Period{Years: 5}, Anchor: "closed_at"},
	} {
		p.Method, p.LegalBasis, p.DescriptionTR, p.Enabled, p.CreatedAt, p.UpdatedAt = privacy.MethodHardDelete, "Hukukçu", "Test", true, now, now
		e.records.PutPolicy(p)
	}
	if got := e.mod.Sweeper.PolicyCodes(); !slices.Equal(got, []string{"breach_record", "destruction_log", "dsr_record"}) {
		t.Fatalf("PolicyCodes() = %v", got)
	}

	// A destruction row written 4 years ago.
	e.clk.Set(now.AddDate(-4, 0, 0))
	if err := e.records.InsertDestructionLog(ctx, kvkk.DestructionLog{ID: uuid.NewV7(), PolicyCode: "notification_delivery",
		RunID: uuid.NewV7(), Module: "notify", Target: "notification_deliveries", Method: privacy.MethodHardDelete,
		PeriodCutoff: e.clk.Now(), StartedAt: e.clk.Now(), FinishedAt: e.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	e.clk.Set(now)
	// A request closed 4 years ago and one closed last month.
	user := uuid.NewV7()
	oldAt, youngAt := now.AddDate(-4, 0, 0), now.AddDate(0, -1, 0)
	var oldDSR, youngDSR uuid.UUID
	for i, at := range []time.Time{oldAt, youngAt} {
		d := kvkk.DSR{ID: uuid.NewV7(), UserID: &user, Type: kvkk.TypeAccess, Channel: kvkk.ChannelInApp, Status: kvkk.StatusReceived,
			Details: "Ayrıntı", ReceivedAt: at, DueAt: at.Add(kvkk.LegalPeriod)}
		if err := e.store.Insert(ctx, d); err != nil {
			t.Fatal(err)
		}
		d.Status, d.CompletedAt = kvkk.StatusCompleted, &at
		if err := e.store.Update(ctx, d); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			oldDSR = d.ID
		} else {
			youngDSR = d.ID
		}
	}
	// A breach closed 6 years ago.
	closed := now.AddDate(-6, 0, 0)
	breach := kvkk.Breach{ID: uuid.NewV7(), Title: "Eski ihlal", DetectedAt: closed, AuthorityDeadlineAt: closed.Add(kvkk.AuthorityPeriod),
		Status: kvkk.BreachClosed, ClosedAt: &closed, OwnerUserID: uuid.NewV7(), CreatedAt: closed, UpdatedAt: closed}
	e.records.PutBreach(breach)

	e.sweepers = []privacy.RetentionSweeper{e.mod.Sweeper}
	if err := e.job(t, kvkk.RetentionSweepJobName).Handle(workerCtx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Get(ctx, oldDSR); err != nil {
		t.Fatalf("a dry run deleted the request: %v", err)
	}
	if _, err := e.records.Breach(ctx, breach.ID); err != nil {
		t.Fatalf("a dry run deleted the breach: %v", err)
	}
	approvePolicies(t, e, kvkk.PolicyDSRRecord, kvkk.PolicyBreachRecord, kvkk.PolicyDestructionLog)
	if err := e.job(t, kvkk.RetentionSweepJobName).Handle(workerCtx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Get(ctx, oldDSR); err == nil {
		t.Fatal("the request closed 4 years ago survived the approved sweep")
	}
	if _, err := e.store.Get(ctx, youngDSR); err != nil {
		t.Fatalf("the request closed last month was purged: %v", err)
	}
	if _, err := e.records.Breach(ctx, breach.ID); err == nil {
		t.Fatal("the breach closed 6 years ago survived the approved sweep")
	}
	logs, err := e.records.DestructionLogs(ctx, kvkk.DestructionFilter{}, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string][]int{}
	for _, l := range logs {
		if l.PolicyCode == "notification_delivery" {
			t.Fatal("the 4-year-old destruction row survived")
		}
		counts[l.PolicyCode] = append(counts[l.PolicyCode], l.RecordCount)
	}
	// Newest first: the approved run, then the dry run (record_count 0).
	for code, want := range map[string][]int{kvkk.PolicyDSRRecord: {1, 0}, kvkk.PolicyBreachRecord: {1, 0}, kvkk.PolicyDestructionLog: {1, 0}} {
		if !slices.Equal(counts[code], want) {
			t.Errorf("%s destruction rows = %v, want %v", code, counts[code], want)
		}
	}
}
