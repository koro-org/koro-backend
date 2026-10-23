//go:build integration

package kvkk_test

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"koro/internal/kvkk"
	"koro/internal/platform/privacy"
	"koro/internal/platform/testutil"
)

// TestDestructionRecorder_Postgres (T-318, BE-083): on PostgreSQL the
// recorder of hooks.destruction writes the destruction_logs row of an
// events export file (the seeded event_export_file policy, owner events)
// in the caller's transaction: committed with it, gone with its rollback;
// another module's policy is refused before the insert.
func TestDestructionRecorder_Postgres(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	records := kvkk.NewGormRecords(d.ORM)
	e := newEnv(t, func(dp *kvkk.Deps) { dp.Records, dp.Tx = records, d.Tx })
	ctx := t.Context()
	now := e.clk.Now()
	rec := func(run uuid.UUID) privacy.Destruction {
		return privacy.Destruction{PolicyCode: "event_export_file", RunID: run, Module: "events",
			Method: privacy.MethodFileDelete, Count: 1, PeriodCutoff: now, StartedAt: now, FinishedAt: now,
			Details: map[string]any{"job": "events.purge_exports", "file_id": uuid.NewV7().String()}}
	}
	committed := uuid.NewV7()
	if err := d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return e.mod.Destructions.Record(ctx, rec(committed))
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rows, err := records.DestructionLogsOfRun(ctx, committed)
	if err != nil || len(rows) != 1 {
		t.Fatalf("committed rows = %+v, %v", rows, err)
	}
	if r := rows[0]; r.PolicyCode != "event_export_file" || r.Module != "events" || r.Target != "files:event_export" ||
		r.Method != privacy.MethodFileDelete || r.RecordCount != 1 || r.Details["job"] != "events.purge_exports" {
		t.Fatalf("destruction row = %+v", r)
	}
	rolledBack, boom := uuid.NewV7(), errors.New("the destruction failed")
	err = d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := e.mod.Destructions.Record(ctx, rec(rolledBack)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("rolled-back transaction = %v", err)
	}
	if rows, err := records.DestructionLogsOfRun(ctx, rolledBack); err != nil || len(rows) != 0 {
		t.Fatalf("a rolled-back destruction left rows: %+v, %v", rows, err)
	}
	foreign := rec(uuid.NewV7())
	foreign.Module = "feed"
	if err := e.mod.Destructions.Record(ctx, foreign); err == nil {
		t.Fatal("feed recorded events' policy")
	}
}
