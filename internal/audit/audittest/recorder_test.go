package audittest_test

import (
	"errors"
	"testing"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
)

func TestRecorder_LogsFiltersAndFails(t *testing.T) {
	t.Parallel()
	var r audittest.Recorder
	ctx := t.Context()
	for _, a := range []string{"x.a", "x.b", "x.a"} {
		if err := r.Log(ctx, audit.Event{Action: a}); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.All()) != 3 || len(r.Of(authz.Action("x.a"))) != 2 || len(r.Of(authz.Action("x.c"))) != 0 {
		t.Fatalf("All = %v", r.All())
	}
	all := r.All()
	all[0].Action = "changed"
	if r.All()[0].Action != "x.a" {
		t.Fatal("All returned the recorder's own slice")
	}

	boom := errors.New("audit down")
	r.FailOn(authz.Action("x.b"), boom)
	if err := r.Log(ctx, audit.Event{Action: "x.b"}); !errors.Is(err, boom) {
		t.Fatalf("FailOn: Log = %v", err)
	}
	if err := r.Log(ctx, audit.Event{Action: "x.a"}); err != nil {
		t.Fatalf("FailOn another action: Log = %v", err)
	}
	r.Fail(boom)
	if err := r.Log(ctx, audit.Event{Action: "x.a"}); !errors.Is(err, boom) {
		t.Fatalf("Fail: Log = %v", err)
	}
	r.Fail(nil)
	r.FailOn("", nil)
	if err := r.Log(ctx, audit.Event{Action: "x.b"}); err != nil {
		t.Fatalf("cleared: Log = %v", err)
	}
	if n := len(r.All()); n != 5 {
		t.Fatalf("recorded %d events, want 5 (failed ones are not recorded)", n)
	}
	r.Reset()
	if r.All() != nil {
		t.Fatal("Reset kept events")
	}
}

func TestRecorder_OutcomesActionsRows(t *testing.T) {
	t.Parallel()
	var r audittest.Recorder
	ctx := t.Context()
	_ = r.Log(ctx, audit.Event{Action: "x.a", Outcome: audit.Success})
	_ = r.Log(ctx, audit.Event{Action: "x.a", Outcome: audit.Denied, Reason: "r"})
	_ = r.Log(ctx, audit.Event{Action: "x.b", Outcome: audit.Success})
	if got := r.OfOutcome("x.a", audit.Denied); len(got) != 1 || got[0].Reason != "r" {
		t.Fatalf("OfOutcome = %v", got)
	}
	if got := r.Actions(); len(got) != 3 || got[0] != "x.a" || got[2] != "x.b" {
		t.Fatalf("Actions = %v", got)
	}
	if r.Last().Action != "x.b" || (&audittest.Recorder{}).Last().Action != "" {
		t.Fatalf("Last = %+v", r.Last())
	}
	rows := r.Rows("x.a")
	if len(rows) != 2 || rows[1].Outcome != string(audit.Denied) || rows[1].Reason != "r" || rows[1].Action != "x.a" {
		t.Fatalf("Rows = %+v", rows)
	}
}
