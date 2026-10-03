package kvkk_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/kvkk"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// recordsWorld is one fresh RecordStore with the seeded policies
// notification_delivery and dsr_erasure; user creates a users row.
type recordsWorld struct {
	s    kvkk.RecordStore
	tx   db.TxManager
	user func(t *testing.T) uuid.UUID
}

// recordsContract is the contract both record stores pass (05 §13.3).
func recordsContract(t *testing.T, newWorld func(t *testing.T) recordsWorld) {
	t.Helper()
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

	t.Run("policies", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		ps, err := w.s.Policies(ctx)
		if err != nil || !slices.ContainsFunc(ps, func(p kvkk.Policy) bool { return p.Code == "notification_delivery" }) ||
			!slices.IsSortedFunc(ps, func(a, b kvkk.Policy) int { return compare(a.Code, b.Code) }) {
			t.Fatalf("Policies = %d, %v", len(ps), err)
		}
		if _, err := w.s.Policy(ctx, "no_such_policy"); !errors.Is(err, problem.ErrNotFound) {
			t.Fatalf("Policy(missing) = %v", err)
		}
		p, err := w.s.Policy(ctx, "notification_delivery")
		if err != nil || p.Period != (kvkk.Period{Years: 1}) || p.Method != "hard_delete" || p.LegalApproved {
			t.Fatalf("Policy = %+v, %v", p, err)
		}
		u := w.user(t)
		err = w.tx.WithTx(ctx, func(ctx context.Context) error {
			l, err := w.s.LockPolicy(ctx, p.Code)
			if err != nil {
				return err
			}
			l.Period, l.LegalApproved, l.ApprovedRef, l.UpdatedBy = kvkk.Period{Years: 1, Months: 1, Days: 2, Hours: 3}, true, "Ref 1", &u
			return w.s.UpdatePolicy(ctx, l)
		})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := w.s.Policy(ctx, p.Code)
		if got.Period != (kvkk.Period{Years: 1, Months: 1, Days: 2, Hours: 3}) || !got.LegalApproved || got.ApprovedRef != "Ref 1" ||
			*got.UpdatedBy != u || !got.UpdatedAt.After(p.UpdatedAt) {
			t.Fatalf("after update = %+v", got)
		}
		got.ApprovedRef = ""
		if err := w.s.UpdatePolicy(ctx, got); !errors.Is(err, kvkk.ErrInvalidRequest) {
			t.Fatalf("approved without a reference = %v", err)
		}
		if err := w.s.UpdatePolicy(ctx, kvkk.Policy{Code: "no_such_policy", Method: "hard_delete"}); !errors.Is(err, problem.ErrNotFound) {
			t.Fatalf("UpdatePolicy(missing) = %v", err)
		}
	})

	t.Run("destruction logs", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		run := uuid.NewV7()
		var ids []uuid.UUID
		for i, code := range []string{"notification_delivery", kvkk.ErasurePolicyCode, "notification_delivery"} {
			l := kvkk.DestructionLog{ID: uuid.NewV7(), PolicyCode: code, RunID: run, Module: "notify", Target: "notification_deliveries",
				Method: "hard_delete", RecordCount: i, PeriodCutoff: base, StartedAt: base, FinishedAt: base,
				Details: map[string]any{"dry_run": false, "eligible": float64(i)}}
			if err := w.s.InsertDestructionLog(ctx, l); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, l.ID)
		}
		bad := kvkk.DestructionLog{ID: uuid.NewV7(), PolicyCode: "no_such_policy", RunID: run, Module: "x", Target: "x",
			Method: "hard_delete", PeriodCutoff: base, StartedAt: base, FinishedAt: base}
		if err := w.s.InsertDestructionLog(ctx, bad); !errors.Is(err, kvkk.ErrInvalidRequest) {
			t.Fatalf("unknown policy = %v", err)
		}
		all, err := w.s.DestructionLogs(ctx, kvkk.DestructionFilter{}, nil, 10)
		if err != nil || len(all) != 3 {
			t.Fatalf("DestructionLogs = %d, %v", len(all), err)
		}
		if all[0].ID != ids[2] || all[0].Details["eligible"] != float64(2) || all[2].ID != ids[0] {
			t.Fatalf("not newest first: %+v", all)
		}
		only, _ := w.s.DestructionLogs(ctx, kvkk.DestructionFilter{PolicyCode: "notification_delivery"}, nil, 10)
		if len(only) != 2 {
			t.Fatalf("filtered = %d", len(only))
		}
		first, _ := w.s.DestructionLogs(ctx, kvkk.DestructionFilter{}, nil, 2)
		rest, _ := w.s.DestructionLogs(ctx, kvkk.DestructionFilter{}, &kvkk.After{At: first[1].CreatedAt, ID: first[1].ID}, 2)
		if len(first) != 2 || len(rest) != 1 || rest[0].ID == first[0].ID || rest[0].ID == first[1].ID {
			t.Fatalf("paging = %v / %v", first, rest)
		}
		future := base.AddDate(10, 0, 0)
		if none, _ := w.s.DestructionLogs(ctx, kvkk.DestructionFilter{From: &future}, nil, 10); len(none) != 0 {
			t.Fatalf("from the future = %d rows", len(none))
		}
		ofRun, _ := w.s.DestructionLogsOfRun(ctx, run)
		if len(ofRun) != 3 {
			t.Fatalf("of run = %d", len(ofRun))
		}
	})

	t.Run("breaches", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		owner := w.user(t)
		n := 12
		b := kvkk.Breach{ID: uuid.NewV7(), Title: "İhlal", DetectedAt: base, AuthorityDeadlineAt: base.Add(kvkk.AuthorityPeriod),
			DataCategories: []string{"contact"}, AffectedEstimate: &n, Description: "Açıklama", Status: kvkk.BreachOpen, OwnerUserID: owner}
		if err := w.s.InsertBreach(ctx, b); err != nil {
			t.Fatal(err)
		}
		got, err := w.s.Breach(ctx, b.ID)
		if err != nil || got.Title != "İhlal" || !got.AuthorityDeadlineAt.Equal(b.AuthorityDeadlineAt) || *got.AffectedEstimate != 12 ||
			!slices.Equal(got.DataCategories, []string{"contact"}) || got.CreatedAt.IsZero() {
			t.Fatalf("Breach = %+v, %v", got, err)
		}
		bad := b
		bad.ID, bad.Status = uuid.NewV7(), kvkk.BreachClosed
		if err := w.s.InsertBreach(ctx, bad); err == nil {
			t.Fatal("a closed breach without closed_at was accepted")
		}
		err = w.tx.WithTx(ctx, func(ctx context.Context) error {
			l, err := w.s.LockBreach(ctx, b.ID)
			if err != nil {
				return err
			}
			at := base.Add(time.Hour)
			l.Status, l.ClosedAt, l.AuthorityNotifiedAt, l.DataCategories = kvkk.BreachClosed, &at, &at, []string{"contact", "identity"}
			return w.s.UpdateBreach(ctx, l)
		})
		if err != nil {
			t.Fatal(err)
		}
		closed := kvkk.BreachClosed
		open := kvkk.BreachOpen
		if cs, _ := w.s.Breaches(ctx, &closed); len(cs) != 1 || cs[0].ClosedAt == nil || len(cs[0].DataCategories) != 2 {
			t.Fatalf("closed breaches = %+v", cs)
		}
		if os, _ := w.s.Breaches(ctx, &open); len(os) != 0 {
			t.Fatalf("open breaches = %+v", os)
		}
		if _, err := w.s.Breach(ctx, uuid.NewV7()); !errors.Is(err, problem.ErrNotFound) {
			t.Fatalf("Breach(missing) = %v", err)
		}
	})

	t.Run("subprocessors", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		d := civil.Date{Year: 2026, Month: time.September, Day: 1}
		x := kvkk.Subprocessor{ID: uuid.NewV7(), Name: "Bulut", Service: "Depolama", DataCategories: []string{"files"},
			Region: "eu-west-1", CountryCode: "IE", TransferMechanism: "standard_contract", ContractSignedAt: &d, Active: true}
		if err := w.s.InsertSubprocessor(ctx, x); err != nil {
			t.Fatal(err)
		}
		dup := x
		dup.ID = uuid.NewV7()
		if err := w.s.InsertSubprocessor(ctx, dup); !errors.Is(err, kvkk.ErrDuplicateSubprocessor) {
			t.Fatalf("duplicate name = %v", err)
		}
		dom := x
		dom.ID, dom.Name, dom.TransferMechanism = uuid.NewV7(), "Yurt dışı", "domestic"
		if err := w.s.InsertSubprocessor(ctx, dom); !errors.Is(err, kvkk.ErrInvalidRequest) {
			t.Fatalf("domestic abroad = %v", err)
		}
		got, err := w.s.Subprocessor(ctx, x.ID)
		if err != nil || got.ContractSignedAt == nil || *got.ContractSignedAt != d || got.AuthorityNotifiedAt != nil {
			t.Fatalf("Subprocessor = %+v, %v", got, err)
		}
		got.Active, got.Notes = false, "Bitti"
		if err := w.s.UpdateSubprocessor(ctx, got); err != nil {
			t.Fatal(err)
		}
		no := false
		if xs, _ := w.s.Subprocessors(ctx, &no); len(xs) != 1 || xs[0].Notes != "Bitti" {
			t.Fatalf("retired = %+v", xs)
		}
	})
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func TestRecords_Contract_Mem(t *testing.T) {
	t.Parallel()
	recordsContract(t, func(t *testing.T) recordsWorld {
		r := kvkk.NewMemRecords(clock.Real().Now)
		seedPolicies(r, time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC))
		return recordsWorld{s: r, tx: db.NopTx{}, user: func(*testing.T) uuid.UUID { return uuid.NewV7() }}
	})
}
