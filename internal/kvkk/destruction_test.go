package kvkk_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/kvkk"
	"koro/internal/platform/privacy"
)

// eventExportPolicy is the seed row of events' export files (000090).
func eventExportPolicy(at time.Time) kvkk.Policy {
	return kvkk.Policy{Code: "event_export_file", OwnerModule: "events", Target: "files:event_export",
		Period: kvkk.Period{Days: 7}, Anchor: "expires_at", Method: privacy.MethodFileDelete, LegalBasis: "R-003 §2.6",
		Enabled: true, DescriptionTR: "Test", CreatedAt: at, UpdatedAt: at}
}

// TestDestructionRecorder_RecordsModuleDestruction (T-318, BE-083): the
// recorder kvkk hands to hooks.destruction appends one destruction_logs
// row of the module's own policy: the policy's target when none is given,
// the caller's run, count, cut-off, times and (copied) details.
func TestDestructionRecorder_RecordsModuleDestruction(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	now := e.clk.Now()
	e.records.PutPolicy(eventExportPolicy(now))
	run, file := uuid.NewV7(), uuid.NewV7()
	details := map[string]any{"job": "events.purge_exports", "file_id": file.String()}
	err := e.mod.Destructions.Record(t.Context(), privacy.Destruction{PolicyCode: "event_export_file", RunID: run,
		Module: "events", Method: privacy.MethodFileDelete, Count: 1, PeriodCutoff: now, StartedAt: now,
		FinishedAt: now.Add(time.Second), Details: details})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	details["file_id"] = "changed after the call"
	rows, err := e.records.DestructionLogsOfRun(t.Context(), run)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows of the run = %+v, %v", rows, err)
	}
	r := rows[0]
	if r.PolicyCode != "event_export_file" || r.Module != "events" || r.Target != "files:event_export" ||
		r.Method != privacy.MethodFileDelete || r.RecordCount != 1 || !r.PeriodCutoff.Equal(now) || !r.StartedAt.Equal(now) ||
		!r.FinishedAt.Equal(now.Add(time.Second)) || r.Details["job"] != "events.purge_exports" || r.Details["file_id"] != file.String() {
		t.Fatalf("destruction row = %+v", r)
	}
	// Zero times are now; the row lists with the others (adminListDestructionLogs).
	run2 := uuid.NewV7()
	if err := e.mod.Destructions.Record(t.Context(), privacy.Destruction{PolicyCode: "event_export_file", RunID: run2,
		Module: "events", Method: privacy.MethodFileDelete, Count: 2}); err != nil {
		t.Fatal(err)
	}
	rows, err = e.records.DestructionLogsOfRun(t.Context(), run2)
	if err != nil || len(rows) != 1 || !rows[0].StartedAt.Equal(now) || !rows[0].FinishedAt.Equal(now) ||
		!rows[0].PeriodCutoff.Equal(now) || rows[0].Details == nil {
		t.Fatalf("row with zero times = %+v, %v", rows, err)
	}
	p, err := e.svc.ListDestructionLogs(t.Context(), actor(t, "dpo"), kvkk.DestructionFilter{PolicyCode: "event_export_file"}, nil, 10)
	if err != nil || len(p.Items) != 2 {
		t.Fatalf("listed rows = %+v, %v", p, err)
	}
}

// TestDestructionRecorder_RejectsMalformed: a module records only its own
// policy, with a run, a known method and a non-negative count; nothing is
// written otherwise (the caller's destruction rolls back with the error).
func TestDestructionRecorder_RejectsMalformed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.records.PutPolicy(eventExportPolicy(e.clk.Now()))
	ok := privacy.Destruction{PolicyCode: "event_export_file", RunID: uuid.NewV7(), Module: "events",
		Method: privacy.MethodFileDelete, Count: 1}
	for name, mut := range map[string]func(*privacy.Destruction){
		"another module's policy": func(d *privacy.Destruction) { d.Module = "feed" },
		"kvkk's policy by events": func(d *privacy.Destruction) { d.PolicyCode = kvkk.ExportPolicyCode },
		"unknown policy":          func(d *privacy.Destruction) { d.PolicyCode = "no_such_policy" },
		"no policy":               func(d *privacy.Destruction) { d.PolicyCode = "" },
		"no module":               func(d *privacy.Destruction) { d.Module = "" },
		"no run":                  func(d *privacy.Destruction) { d.RunID = uuid.Nil() },
		"unknown method":          func(d *privacy.Destruction) { d.Method = "shred" },
		"negative count":          func(d *privacy.Destruction) { d.Count = -1 },
	} {
		d := ok
		mut(&d)
		if err := e.mod.Destructions.Record(t.Context(), d); err == nil {
			t.Errorf("%s: recorded", name)
		}
	}
	p, err := e.svc.ListDestructionLogs(t.Context(), actor(t, "dpo"), kvkk.DestructionFilter{}, nil, 50)
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("rows after the refusals = %+v, %v", p.Items, err)
	}
}
