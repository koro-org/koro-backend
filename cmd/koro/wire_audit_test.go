package main

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"koro/internal/audit"
	"koro/internal/authz/authztest"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
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

// devAuditSeed / devAuditPub are the .env.example audit placeholders.
const (
	devAuditSeed = "dev1:YXVkaXRhdWRpdGF1ZGl0YXVkaXRhdWRpdGF1ZGl0YXU="
	devAuditPub  = "dev1:eARyIQn7PbU7ZruoCTHgf7ojtVhotKNa+pBr0/m6gEI="
)

// BE-107: the API (no root section) gets a public-key verifier only; the
// worker signs and archives, and refuses seeds that do not match the
// public keys the API verifies with.
func TestWire_Audit_RootKeysByMode(t *testing.T) {
	t.Parallel()
	pub := config.Audit{RootVerifyKeys: devAuditPub}
	root := func(seed string) *config.AuditRoot {
		var s config.Secret
		if err := s.UnmarshalText([]byte(seed)); err != nil {
			t.Fatal(err)
		}
		return &config.AuditRoot{RootSigningKeys: s, RootArchiveDir: t.TempDir()}
	}

	v, s, a, err := (&auditWiring{}).rootKeys(pub)
	if err != nil || v == nil || s != nil || a != nil {
		t.Fatalf("api rootKeys() = %v, %v, %v, %v; want a verifier only", v, s, a, err)
	}
	v, s, a, err = (&auditWiring{root: root(devAuditSeed)}).rootKeys(pub)
	if err != nil || v == nil || s == nil || a == nil {
		t.Fatalf("worker rootKeys() = %v, %v, %v, %v", v, s, a, err)
	}
	v, s, _, err = (&auditWiring{root: root(devAuditSeed)}).rootKeys(config.Audit{})
	if err != nil || v != nil || s == nil {
		t.Fatalf("dev worker without public keys rootKeys() = %v, %v, %v; want the signer to verify", v, s, err)
	}
	other := "dev1:" + base64.StdEncoding.EncodeToString([]byte("another-wiring-test-root-seed-32"))
	if _, _, _, err := (&auditWiring{root: root(other)}).rootKeys(pub); err == nil || !strings.Contains(err.Error(), "not the public key of signing key dev1") {
		t.Fatalf("mismatched seed rootKeys() error = %v", err)
	}
	if _, _, _, err := (&auditWiring{root: root("k2:" + strings.TrimPrefix(devAuditSeed, "dev1:"))}).rootKeys(pub); err == nil ||
		!strings.Contains(err.Error(), "signing key k2 has no AUDIT_ROOT_VERIFY_KEYS entry") {
		t.Fatalf("seed without a public key rootKeys() error = %v", err)
	}
}

// The .env.example public key is the public key of its development seed,
// so `make dev` starts the worker and the API verifies its roots.
func TestEnvExample_AuditVerifyKeysMatchSigningKeys(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]string{}
	for l := range strings.SplitSeq(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && strings.HasPrefix(k, "AUDIT_ROOT_") {
			vals[k] = v
		}
	}
	if vals["AUDIT_ROOT_SIGNING_KEYS"] != devAuditSeed || vals["AUDIT_ROOT_VERIFY_KEYS"] != devAuditPub {
		t.Fatalf(".env.example audit keys = %q", vals)
	}
	seeds, err := crypto.ParseKeyring(vals["AUDIT_ROOT_SIGNING_KEYS"])
	if err != nil {
		t.Fatal(err)
	}
	pubs, err := crypto.ParseKeyring(vals["AUDIT_ROOT_VERIFY_KEYS"])
	if err != nil {
		t.Fatal(err)
	}
	s, err := audit.NewEd25519Signer(seeds)
	if err != nil {
		t.Fatal(err)
	}
	v, err := audit.NewEd25519Verifier(pubs)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.CheckCovers(s); err != nil {
		t.Fatal(err)
	}
}

// In staging/prod the API warns (names only, never values) when it is
// handed the worker-only root variables; dev shares one .env and stays quiet.
func TestWire_Audit_APIWarnsOnWorkerOnlyVars(t *testing.T) {
	t.Parallel()
	environ := map[string]string{"AUDIT_ROOT_SIGNING_KEYS": devAuditSeed, "AUDIT_ROOT_ARCHIVE_DIR": "/x"}
	for _, tc := range []struct {
		env  string
		want int
	}{{config.EnvProd, 2}, {config.EnvStaging, 2}, {config.EnvDev, 0}} {
		var buf bytes.Buffer
		warnIgnoredAPIVars(t.Context(), slog.New(slog.NewJSONHandler(&buf, nil)), config.Common{Env: tc.env}, environ)
		out := buf.String()
		if got := strings.Count(out, "used only by koro worker"); got != tc.want {
			t.Errorf("%s: %d warnings, want %d: %s", tc.env, got, tc.want, out)
		}
		if strings.Contains(out, "YXVka") {
			t.Fatalf("%s: warning leaks the seed: %s", tc.env, out)
		}
	}
}
