//go:build integration

package outbox_test

import (
	"testing"
	"time"

	"koro/internal/platform/outbox"
	"koro/internal/platform/testutil"
)

// TestOutbox_ReadBacklog_PendingLagAndDead: the backlog counts unpublished
// rows, the age of the oldest one and the rows past AlarmAttempts;
// published rows do not count.
func TestOutbox_ReadBacklog_PendingLagAndDead(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

	empty, err := outbox.ReadBacklog(ctx, d.ORM, now)
	if err != nil || empty != (outbox.Backlog{}) {
		t.Fatalf("empty backlog = %+v, %v", empty, err)
	}
	for _, row := range []struct {
		occurred  time.Time
		attempts  int
		published bool
	}{
		{now.Add(-90 * time.Second), 0, false},
		{now.Add(-30 * time.Second), outbox.AlarmAttempts, false},
		{now.Add(-time.Hour), 0, true},
	} {
		var pub *time.Time
		if row.published {
			pub = &now
		}
		if _, err := d.Pool.Exec(ctx, `INSERT INTO outbox (topic, aggregate_type, aggregate_id, payload, occurred_at, attempts, published_at)
			VALUES ('file.uploaded', 'file', uuidv7(), '{}', $1, $2, $3)`, row.occurred, row.attempts, pub); err != nil {
			t.Fatal(err)
		}
	}
	got, err := outbox.ReadBacklog(ctx, d.ORM, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := (outbox.Backlog{Pending: 2, Dead: 1, OldestPending: 90 * time.Second}); got != want {
		t.Fatalf("backlog = %+v, want %+v", got, want)
	}
}
