//go:build integration

package netgsm_test

import (
	"os"
	"testing"
	"time"

	"koro/internal/platform/db"
	"koro/internal/platform/iys/netgsm"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// TestGormStore_Contract: iys_netgsm_events (000147) through app_rw
// behaves like the in-memory store.
func TestGormStore_Contract(t *testing.T) {
	t.Parallel()
	runStoreContract(t, func(t *testing.T) netgsm.Store { return netgsm.NewGormStore(testutil.NewDB(t).ORM) })
}

// TestGormStore_Constraints: the table refuses what the adapter never
// writes (a change with a result, a change without a date, a malformed
// key) and app_rw cannot change a stored item.
func TestGormStore_Constraints(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	at := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	ins := func(kind, eventKey, result string, consentAt *time.Time) error {
		var res *string
		if result != "" {
			res = &result
		}
		return d.ORM.WithContext(ctx).Exec(`INSERT INTO iys_netgsm_events (feed, event_key, kind, type, status, result, consent_at, received_at)
			VALUES ('netgsm:1/2', ?, ?, 'EPOSTA', 'RET', ?, ?, ? )`, eventKey, kind, res, consentAt, at).Error
	}
	for name, err := range map[string]error{
		"change with a result":  ins("change", key(1), "success", &at),
		"change without a date": ins("change", key(2), "", nil),
		"result without result": ins("result", key(3), "", &at),
		"malformed key":         ins("change", "not-a-digest", "", &at),
	} {
		if !db.IsCheck(err) {
			t.Errorf("%s: %v, want a check violation", name, err)
		}
	}
	if err := ins("change", key(4), "", &at); err != nil {
		t.Fatal(err)
	}
	if err := d.ORM.WithContext(ctx).Exec(`UPDATE iys_netgsm_events SET status = 'ONAY'`).Error; err == nil {
		t.Fatal("app_rw updated a stored item")
	}
}
