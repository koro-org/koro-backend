package notify_test

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"koro/internal/notify"
	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// countingTx counts the outermost transactions a sweep opens (NopTx
// otherwise).
type countingTx struct {
	db.NopTx
	n int
}

func (c *countingTx) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if !db.InTx(ctx) {
		c.n++
	}
	return c.NopTx.WithTx(ctx, fn)
}

func TestSweeper_PolicyCodes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	want := []string{notify.PolicyEmailSuppression, notify.PolicyInAppNotification, notify.PolicyNotificationDelivery, notify.PolicyPushToken}
	if got := f.mod.Sweeper.PolicyCodes(); !slices.Equal(got, want) {
		t.Fatalf("PolicyCodes() = %v, want %v", got, want)
	}
	if _, err := f.mod.Sweeper.Sweep(t.Context(), privacy.RetentionPolicy{Code: "talent_ledger", LegalApproved: true}, t0); err == nil {
		t.Fatal("a policy of another module was swept")
	}
}

// TestSweeper_DryRunThenBatchesOf1000: unapproved, the sweep only counts;
// approved, 2500 expired in-app notifications go in three transactions of
// at most 1000 rows and the young one stays.
func TestSweeper_DryRunThenBatchesOf1000(t *testing.T) {
	t.Parallel()
	tx := &countingTx{}
	f := newFixture(t, func(d *notify.Deps) { d.Tx = tx })
	ctx := t.Context()
	user := newID()
	for i := range 2501 {
		at := t0.AddDate(-1, 0, 0).Add(-time.Duration(i) * time.Minute)
		if i == 2500 {
			at = t0
		}
		if _, err := f.store.InsertInApp(ctx, notify.InAppNotification{ID: newID(), UserID: user, Kind: "notify.test_message",
			Title: "Başlık " + strconv.Itoa(i), CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	p := privacy.RetentionPolicy{Code: notify.PolicyInAppNotification, Retention: 180 * 24 * time.Hour, Method: privacy.MethodHardDelete}
	res, err := f.mod.Sweeper.Sweep(ctx, p, t0)
	if err != nil || res.Count != 2500 || res.Table != "in_app_notifications" || tx.n != 0 {
		t.Fatalf("dry run = %+v, %v (%d transactions)", res, err, tx.n)
	}
	if n, _ := f.store.PurgeExpired(ctx, p.Code, t0.Add(time.Hour), 0, true); n != 2501 {
		t.Fatalf("the dry run deleted rows: %d left", n)
	}
	p.LegalApproved = true
	res, err = f.mod.Sweeper.Sweep(ctx, p, t0)
	if err != nil || res.Count != 2500 || tx.n != 3 {
		t.Fatalf("approved sweep = %+v, %v in %d transactions; want 2500 in 3", res, err, tx.n)
	}
	if n, _ := f.store.PurgeExpired(ctx, p.Code, t0.Add(time.Hour), 0, true); n != 1 {
		t.Fatalf("rows left = %d, want the young one", n)
	}
	// Idempotent: a rerun finds nothing.
	if res, err = f.mod.Sweeper.Sweep(ctx, p, t0); err != nil || res.Count != 0 {
		t.Fatalf("rerun = %+v, %v", res, err)
	}
}
