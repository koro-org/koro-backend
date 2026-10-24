package events_test

import (
	"context"
	"testing"
	"uuid"

	"koro/internal/events"
)

// TestLiveStream_DropsStaleCounters (T-325): two check-ins committing at
// once publish their counters in either order; a connection drops a
// snapshot or stats message stamped before the last counters it got, so
// the board never steps back to the earlier read. Scans always pass;
// equal stamps pass; the names filter still applies after the guard.
func TestLiveStream_DropsStaleCounters(t *testing.T) {
	t.Parallel()
	b := events.LiveBoard{EventID: uuid.NewV7(), OrgID: uuid.NewV7(), Names: false}
	snap, filter := events.LiveStream(b, func(context.Context) ([]byte, error) {
		return []byte(`{"type":"snapshot","checked_in":5,"at":"2026-11-20T10:00:02Z"}`), nil
	})
	if _, err := snap(t.Context()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, msg, want string
		keep            bool
	}{
		{"stats read before the snapshot", `{"type":"stats","checked_in":4,"at":"2026-11-20T10:00:01Z"}`, "", false},
		{"stats with the snapshot's stamp", `{"type":"stats","checked_in":5,"at":"2026-11-20T10:00:02Z"}`,
			`{"type":"stats","checked_in":5,"at":"2026-11-20T10:00:02Z"}`, true},
		{"fresh stats (B's read, after both commits)", `{"type":"stats","checked_in":7,"at":"2026-11-20T10:00:04Z"}`,
			`{"type":"stats","checked_in":7,"at":"2026-11-20T10:00:04Z"}`, true},
		{"a scan stamped earlier still passes, without the name",
			`{"type":"checkin","checkin_id":"c1","attendee_display":"Ayşe K.","method":"qr","at":"2026-11-20T10:00:03Z"}`,
			`{"at":"2026-11-20T10:00:03Z","checkin_id":"c1","method":"qr","type":"checkin"}`, true},
		{"A's stale stats arriving after B's", `{"type":"stats","checked_in":6,"at":"2026-11-20T10:00:03Z"}`, "", false},
		{"not JSON", `nope`, "", false},
	}
	for _, c := range cases { // in order: the guard is stateful
		got, ok := filter([]byte(c.msg))
		if ok != c.keep || (ok && string(got) != c.want) {
			t.Fatalf("%s: %s, %v; want %s, %v", c.name, got, ok, c.want, c.keep)
		}
	}

	// Each connection has its own guard.
	_, other := events.LiveStream(events.LiveBoard{Names: true}, nil)
	if _, ok := other([]byte(`{"type":"stats","at":"2026-11-20T10:00:01Z"}`)); !ok {
		t.Fatal("a new connection's guard dropped its first counters")
	}
}
