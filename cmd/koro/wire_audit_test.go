package main

import (
	"slices"
	"testing"
	"time"

	"koro/internal/audit"
	"koro/internal/authz/authztest"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
)

// useMemStore installs the in-memory audit stores used by service,
// wiring and authz matrix tests (05 §13.2). Owner: BE-007.
// Only tickets of the audit module edit this file (BE-001 R4).
func (w *auditWiring) useMemStore() {
	w.store = audit.NewMemStore()
	s, err := audit.NewEd25519Signer([]crypto.Key{{ID: "test1", Secret: []byte("koro-wiring-test-root-seed-32byt")}})
	if err != nil {
		panic(err)
	}
	w.signer, w.archive = s, audit.NewMemArchive()
}

func TestWire_Audit_JobsScheduledAsDesigned(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, j := range c.infra.jobs.Jobs() {
		if slices.Contains([]string{audit.JobChain, audit.JobDailyRoot, audit.JobVerifyChain}, j.Name) {
			got = append(got, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	// 01 §7 rows of audit.
	want := []string{"audit.chain @every 10s critical", "audit.daily_root 15 0 * * * default", "audit.verify_chain 0 5 * * * low"}
	if !slices.Equal(got, want) {
		t.Fatalf("audit jobs %q, want %q", got, want)
	}
	mux, err := c.workerMux(c.infra.log)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{audit.JobChain, audit.JobDailyRoot, audit.JobVerifyChain} {
		if !slices.Contains(mux.Types(), name) {
			t.Errorf("worker does not route %s", name)
		}
	}
}

// The wired service logs, the chain job links the rows, the daily root is
// signed and archived, and verification over the wired store passes.
func TestWire_Audit_LogChainRootVerify(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	admin := authztest.ID("platform_admin")
	for range 3 {
		if err := c.audit.svc.Log(ctx, audit.Event{Action: "identity.session.create", Outcome: audit.Success, ActorUserID: &admin}); err != nil {
			t.Fatal(err)
		}
	}
	run := func(name string) {
		t.Helper()
		for _, j := range c.infra.jobs.Jobs() {
			if j.Name == name {
				if err := j.Handle(ctx); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				return
			}
		}
		t.Fatalf("job %s not scheduled", name)
	}
	run(audit.JobChain)
	c.infra.clock.(*clock.Fake).Advance(24 * time.Hour)
	run(audit.JobDailyRoot)
	run(audit.JobVerifyChain)

	pad, _ := authztest.Get("platform_admin")
	today := c.infra.clock.Now()
	res, err := c.audit.svc.Verify(ctx, pad.Actor, civilDay(today, -1), civilDay(today, 0))
	if err != nil || !res.OK || res.CheckedRows < 3 {
		t.Fatalf("Verify() = %+v, %v; want ok over the three chained rows", res, err)
	}
	if _, ok := c.audit.archive.(*audit.MemArchive).Get("audit-roots/" + civilDay(today, -1).String() + ".json"); !ok {
		t.Fatal("the daily root was not archived")
	}
}

// civilDay is the Europe/Istanbul day of t shifted by offset days.
func civilDay(t time.Time, offset int) civil.Date { return civil.Today(t).AddDays(offset) }
