//go:build integration

package consent_test

import (
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/consent"
)

// TestIYSStatusSync_Gorm_ConcurrentRunsWithdrawOnce (BE-103, T-312) on
// PostgreSQL (000143): the hold refuses the e-mail filter until the first
// reconciliation; two runs racing over the same İYS refusal append one
// withdrawal (channel iys, audited, no İYS submission, no outbox event):
// the second waits on the subject lock and finds the grant gone. The
// channel state is written once per complete run.
func TestIYSStatusSync_Gorm_ConcurrentRunsWithdrawOnce(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t, func(d *consent.Deps) { d.IYSSyncMaxAge = 6 * time.Hour })
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeCommercialEmail)
	users := []uuid.UUID{insertUser(t, e.d, uuid.NewV7()), insertUser(t, e.d, uuid.NewV7()), insertUser(t, e.d, uuid.NewV7())}
	grants := map[uuid.UUID]uuid.UUID{}
	for _, u := range users {
		id, err := e.svc.Record(ctx, grant(u, text, consent.ChannelWeb))
		if err != nil {
			t.Fatal(err)
		}
		grants[u] = id
	}
	refuser := users[1]
	e.clk.Advance(time.Minute)
	e.iys.SetStatus(refuser, consent.IYSEmail, consent.IYSState{Status: consent.IYSRefused, At: e.clk.Now(), Source: "IYS_CM"})
	if _, err := e.svc.FilterActive(ctx, users, consent.PurposeCommercialEmail); !errors.Is(err, consent.ErrIYSSyncStale) {
		t.Fatalf("FilterActive before any reconciliation = %v", err)
	}
	outbox := e.count(t, `SELECT count(*) FROM outbox WHERE topic = 'consent.recorded'`)

	var job func() error
	for _, j := range e.mod.Jobs() {
		if j.Name == consent.StatusSyncJobName {
			job = func() error { return j.Handle(ctx) }
		}
	}
	if job == nil {
		t.Fatal("no consent.iys_status_sync")
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { errs[i] = job() })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	if n := e.count(t, `SELECT count(*) FROM consents WHERE user_id = $1 AND channel = 'iys' AND action = 'withdrawn'`, refuser); n != 1 {
		t.Fatalf("İYS withdrawals of the refuser = %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM consents WHERE channel = 'iys'`); n != 1 {
		t.Fatalf("İYS withdrawals = %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM iys_submissions s JOIN consents c ON c.id = s.consent_id WHERE c.channel = 'iys'`); n != 0 {
		t.Fatalf("İYS submissions of İYS withdrawals = %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM outbox WHERE topic = 'consent.recorded'`); n != outbox {
		t.Fatalf("consent.recorded events = %d, want %d (the refusal is not sent back)", n, outbox)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events a JOIN consents c ON c.id = a.resource_id
		WHERE c.channel = 'iys' AND a.action = 'consent.record.create' AND a.metadata->>'iys_reason' = 'refused'
		AND a.metadata->>'grant_consent_id' = $1`, grants[refuser].String()); n != 1 {
		t.Fatalf("audit rows of the İYS withdrawal = %d, want 1", n)
	}
	st, ok, err := e.store.IYSSync(ctx, consent.IYSEmail)
	if err != nil || !ok || st.Checked < 2 || st.Withdrawn > 1 || st.UpdatedAt.IsZero() {
		t.Fatalf("iys_status_syncs(email) = %+v, %v, %v", st, ok, err)
	}
	got, err := e.svc.FilterActive(ctx, users, consent.PurposeCommercialEmail)
	if err != nil || len(got) != 2 || got[0] == refuser || got[1] == refuser {
		t.Fatalf("FilterActive after the reconciliation = %v, %v", got, err)
	}
}
