package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/hibiken/asynq"

	"koro/db/migrations"
	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/kvkk"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
)

// testDPOEmail is the DPO mailbox of wiring and matrix tests.
const testDPOEmail = "kvkk@koro.test"

// useMemStore installs the in-memory kvkk store used by service, wiring and
// authz matrix tests (05 §13.2) and the test re-authentication verifier.
// First owner: BE-021. Only tickets of the kvkk module edit this file
// (BE-001 R4).
func (w *kvkkWiring) useMemStore() {
	w.store = kvkk.NewMemStore(clock.Real().Now)
	records := kvkk.NewMemRecords(clock.Real().Now)
	memPolicies(records)
	w.records = records
	w.tx = db.NopTx{}
	w.cfg = &config.Kvkk{DPOEmail: testDPOEmail, ExportRetention: config.MaxExportRetention}
	w.reauth = testReauth
}

// matrixRecordsAt is the created/updated time of every seeded kvkk-2 record
// of the in-memory store: its ETag is "DDNFR4TI7YAAA" (testdata/authz/kvkk.yaml).
var matrixRecordsAt = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

// memPolicies seeds the retention policies the wiring and matrix tests use,
// as 000090/000121 do (none legally approved).
func memPolicies(r *kvkk.MemRecords) {
	for _, p := range []kvkk.Policy{
		{Code: kvkk.ErasurePolicyCode, OwnerModule: "kvkk", Target: "users", Anchor: "created_at", Method: privacy.MethodHardDelete},
		{Code: kvkk.ExportPolicyCode, OwnerModule: "kvkk", Target: "files:dsr_export", Period: kvkk.Period{Days: 7},
			Anchor: "created_at", Method: privacy.MethodFileDelete},
		{Code: "notification_delivery", OwnerModule: "notify", Target: "notification_deliveries", Period: kvkk.Period{Years: 1},
			Anchor: "created_at", Method: privacy.MethodHardDelete},
		{Code: "outbox_published", OwnerModule: "platform", Target: "outbox", Period: kvkk.Period{Days: 7},
			Anchor: "published_at", Method: privacy.MethodHardDelete},
	} {
		p.LegalBasis, p.DescriptionTR, p.Enabled, p.CreatedAt, p.UpdatedAt = "Öneri", "Test", true, matrixRecordsAt, matrixRecordsAt
		r.PutPolicy(p)
	}
}

// TestWire_KvkkTasksAndPrivacy pins what kvkk registers (01 §6.2, §7): the
// three dsr.* subscriptions on the critical queue, kvkk.dsr_deadlines and
// kvkk.purge_exports, and kvkk's own KVKK provider.
func TestWire_KvkkTasksAndPrivacy(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "kvkk.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"kvkk.dsr_deadlines 0 9 * * * default", "kvkk.purge_exports 27 * * * * low",
		"kvkk.retention_sweep 30 2 * * * low", "kvkk.breach_timer 0 * * * * critical"}; !slices.Equal(jobs, want) {
		t.Fatalf("kvkk jobs %q, want %q", jobs, want)
	}
	for topic, name := range map[string]string{
		domainevent.TopicDSRSubmitted: kvkk.NotifyDPOTaskName, domainevent.TopicDSRErasureApproved: kvkk.EraseAccountTaskName,
		domainevent.TopicDSRCompleted: kvkk.FinishRequestTaskName, domainevent.TopicBreachRecorded: kvkk.AlertBreachTaskName,
	} {
		subs := c.infra.subs.For(topic)
		if !slices.ContainsFunc(subs, func(s outbox.Subscription) bool { return s.Name == name && s.Queue == queue.Critical }) {
			t.Errorf("%s: no %s subscription on the critical queue (%+v)", topic, name, subs)
		}
	}
	mux, err := c.workerMux(obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{kvkk.NotifyDPOTaskName, kvkk.EraseAccountTaskName, kvkk.FinishRequestTaskName,
		kvkk.DeadlinesJobName, kvkk.PurgeExportsJobName, kvkk.RetentionSweepJobName, kvkk.BreachTimerJobName, kvkk.AlertBreachTaskName} {
		if !slices.Contains(mux.Types(), name) {
			t.Errorf("worker does not route %s", name)
		}
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "kvkk" }) {
		t.Fatal("kvkk privacy provider not registered")
	}
	// hooks.retention (01 §7): the platform cleanup reads the period of the
	// policy; an unknown code keeps the configured default.
	if c.hooks.retention == nil {
		t.Fatal("kvkk did not fill hooks.retention")
	}
	if d, found, err := c.hooks.retention(t.Context(), "outbox_published"); err != nil || !found || d != 7*24*time.Hour {
		t.Fatalf("hooks.retention(outbox_published) = %s, %v, %v", d, found, err)
	}
	if _, found, err := c.hooks.retention(t.Context(), "no_such_policy"); err != nil || found {
		t.Fatalf("hooks.retention(unknown) found = %v, %v", found, err)
	}
}

// ---------------------------------------------------------------- T-92

// catalogLine is one line of testdata/kvkk/personal_tables.txt.
type catalogLine struct {
	table, class, owner, coverage string
	line                          int
}

var (
	createTableRe = regexp.MustCompile(`(?i)CREATE TABLE (?:IF NOT EXISTS )?([a-z_]+(?:\.[a-z_]+)?)\s*\(`)
	alterTableRe  = regexp.MustCompile(`(?i)ALTER TABLE (?:ONLY )?([a-z_]+(?:\.[a-z_]+)?)`)
	userRefRe     = regexp.MustCompile(`(?i)REFERENCES (?:public\.)?users\b`)
	userColRe     = regexp.MustCompile(`(?m)^\s*([a-z_]*user_id)\s+uuid`)
	todoRe        = regexp.MustCompile(`^todo:BE-\d{3}$`)
)

// schemaTables reads every table of the embedded migrations and whether it
// has a column that names a user (a users FK, or a *user_id column, which
// the talent schema keeps without FK).
func schemaTables(t *testing.T) map[string]bool {
	t.Helper()
	tables := map[string]bool{}
	err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".up.sql") {
			return err
		}
		b, err := fs.ReadFile(migrations.FS, path)
		if err != nil {
			return err
		}
		for stmt := range strings.SplitSeq(string(b), ";") {
			if m := createTableRe.FindStringSubmatch(stmt); m != nil {
				tables[m[1]] = tables[m[1]] || userRefRe.MatchString(stmt) || userColRe.MatchString(stmt)
				continue
			}
			if m := alterTableRe.FindStringSubmatch(stmt); m != nil && userRefRe.MatchString(stmt) {
				tables[m[1]] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tables
}

func readCatalog(t *testing.T) []catalogLine {
	t.Helper()
	b, err := os.ReadFile("testdata/kvkk/personal_tables.txt")
	if err != nil {
		t.Fatal(err)
	}
	var out []catalogLine
	for i, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		f := strings.Fields(ln)
		if len(f) < 4 {
			t.Fatalf("personal_tables.txt:%d: want <table> <class> <owner> <coverage>", i+1)
		}
		out = append(out, catalogLine{table: f[0], class: f[1], owner: f[2], coverage: strings.Join(f[3:], " "), line: i + 1})
	}
	return out
}

// TestEveryPersonalTableHasProvider (01 §3.3, T-92): every table of the
// schema is classified; every personal (or special category) table is
// covered by a registered privacy.Provider, by a TODO of the module ticket
// that will register it, or by a reasoned exemption; a table naming a user
// is never classified "none". A module that adds a personal table without
// updating its provider and this map breaks the build.
func TestEveryPersonalTableHasProvider(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, p := range c.privacy {
		registered[p.Name()] = true
	}
	schema := schemaTables(t)
	if len(schema) < 70 {
		t.Fatalf("read only %d tables from the migrations", len(schema))
	}
	seen, covered := map[string]bool{}, map[string]bool{}
	for _, l := range readCatalog(t) {
		at := fmt.Sprintf("personal_tables.txt:%d (%s)", l.line, l.table)
		if seen[l.table] {
			t.Errorf("%s: listed twice", at)
		}
		seen[l.table] = true
		names, ok := schema[l.table]
		if !ok {
			t.Errorf("%s: no such table in db/migrations", at)
		}
		switch l.class {
		case "personal", "special":
			switch {
			case strings.HasPrefix(l.coverage, "provider:"):
				name := strings.TrimPrefix(l.coverage, "provider:")
				if !registered[name] {
					t.Errorf("%s: provider %q is not registered in c.privacy", at, name)
				}
				if name != l.owner {
					t.Errorf("%s: provider %q is not the owner module %q", at, name, l.owner)
				}
				covered[name] = true
			case todoRe.MatchString(l.coverage):
			case strings.HasPrefix(l.coverage, "exempt:") && len(strings.TrimPrefix(l.coverage, "exempt:")) >= 20:
			default:
				t.Errorf("%s: personal table without coverage (provider:<name>, todo:BE-NNN or exempt:<reason>)", at)
			}
		case "staff", "none":
			if l.coverage != "-" {
				t.Errorf("%s: a %s table takes coverage \"-\"", at, l.class)
			}
			if l.class == "none" && names {
				t.Errorf("%s: the table names a user but is classified none (personal or staff?)", at)
			}
		default:
			t.Errorf("%s: unknown class %q", at, l.class)
		}
	}
	for table := range schema {
		if !seen[table] {
			t.Errorf("table %s is not in testdata/kvkk/personal_tables.txt: classify it and name its provider (01 §3.3)", table)
		}
	}
	for name := range registered {
		if !covered[name] {
			t.Errorf("provider %s covers no table in personal_tables.txt", name)
		}
	}
}

// ---------------------------------------------------------------- end to end

// drain relays the outbox and runs every queued task through the worker
// mux until nothing new is queued (at-least-once: tasks run once here).
func drain(t *testing.T, c *container) {
	t.Helper()
	mux, err := c.workerMux(obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	q := c.infra.queue.(*queue.Mem)
	done := 0
	for range 20 {
		if _, err := c.infra.outbox.NewRelay(outbox.RelayConfig{}).Once(workerContext(t)); err != nil {
			t.Fatal(err)
		}
		tasks := q.Tasks()
		if len(tasks) == done {
			return
		}
		for _, tk := range tasks[done:] {
			if err := mux.ProcessTask(workerContext(t), asynq.NewTask(tk.Type, tk.Payload)); err != nil {
				t.Fatalf("task %s: %v", tk.Type, err)
			}
		}
		done = len(tasks)
	}
	t.Fatal("the outbox did not drain")
}

// verifyAuditChain recomputes the whole hash chain of the in-memory audit
// store and returns the row hashes by ID.
func verifyAuditChain(t *testing.T, c *container) map[uuid.UUID][]byte {
	t.Helper()
	for _, j := range c.infra.jobs.Jobs() {
		if j.Name == "audit.chain" {
			if err := j.Handle(workerContext(t)); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := c.audit.store.(*audit.MemStore).Chained(t.Context(), 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	prev, out := make([]byte, 32), map[uuid.UUID][]byte{}
	for _, r := range rows {
		h, err := audit.RowHash(prev, r)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(r.PrevHash, prev) || !bytes.Equal(r.RowHash, h) {
			t.Fatalf("audit chain broken at chain_seq %d", *r.ChainSeq)
		}
		prev, out[r.ID] = h, h
	}
	return out
}

// TestErasure_IdentityAnonymizedLast_SessionsRevoked runs an erasure over
// the real in-memory modules: the user's sessions stop resolving, the
// account is a tombstone (still there: foreign keys hold), notify's data is
// gone, and the audit log and its hash chain are untouched.
func TestErasure_IdentityAnonymizedLast_SessionsRevoked(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	ctx := t.Context()
	u := e.user(t, nil)
	web := e.session(t, u, identity.ClientWeb)
	mobile := e.session(t, u, identity.ClientMobile)
	subject := authz.Actor{Kind: authz.KindUser, UserID: u.ID}
	if _, err := e.c.notify.svc.PutMyPreferences(ctx, subject, []notify.Preference{
		{Category: notify.PrefEventUpdates, Channel: notify.ChannelEmail, Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	// Record the erase order of the real providers.
	var mu sync.Mutex
	var order []string
	for i, p := range e.c.privacy {
		e.c.privacy[i] = orderedProvider{Provider: p, record: func(n string) { mu.Lock(); order = append(order, n); mu.Unlock() }}
	}

	d, err := e.c.kvkk.svc.CreateMine(ctx, subject, kvkk.MyRequest{Type: kvkk.TypeErasure, Proof: kvkk.ReauthProof{Password: matrixReauthPassword}})
	if err != nil {
		t.Fatal(err)
	}
	dpo, _ := authztest.Get("dpo")
	if _, err := e.c.kvkk.svc.Complete(ctx, dpo.Actor, d.ID, "Hesabınız silindi.", kvkk.ReauthProof{Password: matrixReauthPassword}); err != nil {
		t.Fatal(err)
	}
	before := verifyAuditChain(t, e.c)
	drain(t, e.c)

	idx := slices.Index(order, "identity")
	if idx < 0 || len(order) != 2*len(e.c.privacy)-1 {
		t.Fatalf("erase order %v", order)
	}
	first := slices.Clone(order[:idx])
	slices.Sort(first)
	var others []string
	for _, p := range e.c.privacy {
		if p.Name() != "identity" {
			others = append(others, p.Name())
		}
	}
	slices.Sort(others)
	if !slices.Equal(first, others) {
		t.Fatalf("before identity: %v, want every other provider %v", first, others)
	}
	for _, raw := range []struct {
		tr  httpx.Transport
		tok string
	}{{httpx.TransportCookie, web}, {httpx.TransportBearer, mobile}} {
		if _, err := e.resolve(ctx, raw.tr, raw.tok); !errors.Is(err, problem.ErrUnauthenticated) {
			t.Fatalf("session after erasure resolves: %v", err)
		}
	}
	if s, _ := e.store.SessionsOf(ctx, u.ID); len(s) != 0 {
		t.Fatalf("%d sessions left", len(s))
	}
	tomb, err := e.store.User(ctx, u.ID)
	if err != nil || tomb.Status != identity.StatusErased || tomb.Email != "" || tomb.FirstName != "" {
		t.Fatalf("account after erasure = %+v, %v (a tombstone must stay)", tomb, err)
	}
	if prefs, _ := e.c.notify.svc.MyPreferences(ctx, subject); slices.ContainsFunc(prefs, func(p notify.Preference) bool {
		return p.Category == notify.PrefEventUpdates && p.Channel == notify.ChannelEmail && !p.Enabled
	}) {
		t.Fatal("notify preferences survived the erasure")
	}
	after := verifyAuditChain(t, e.c)
	for id, h := range before {
		if !bytes.Equal(after[id], h) {
			t.Fatal("an audit row changed or disappeared during the erasure")
		}
	}
	got, _ := e.c.kvkk.svc.AdminGet(ctx, dpo.Actor, d.ID)
	if !strings.Contains(got.ResponseSummary, "Hesap silme raporu") || *got.UserID != u.ID {
		t.Fatalf("request after the run = %+v", got)
	}
	mails := e.c.notify.transport.(*notify.MemTransport).Sent()
	if !slices.ContainsFunc(mails, func(m notify.Email) bool { return m.To == u.Email }) {
		t.Fatal("the subject was not told before the address disappeared")
	}
}

// orderedProvider records erase calls of a real provider.
type orderedProvider struct {
	privacy.Provider
	record func(string)
}

func (p orderedProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	p.record(p.Name())
	return p.Provider.Erase(ctx, userID)
}

// TestExport_ZipContainsAllRegisteredProviders_Wired: the export of a real
// container has one section per registered provider (identity, consent,
// files, notify, kvkk, …) and the file sits in the files module.
func TestExport_ZipContainsAllRegisteredProviders_Wired(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	ctx := t.Context()
	u := e.user(t, nil)
	subject := authz.Actor{Kind: authz.KindUser, UserID: u.ID}
	d, err := e.c.kvkk.svc.CreateMine(ctx, subject, kvkk.MyRequest{Type: kvkk.TypeExport})
	if err != nil {
		t.Fatal(err)
	}
	dpo, _ := authztest.Get("dpo")
	if _, err := e.c.kvkk.svc.Complete(ctx, dpo.Actor, d.ID, "Verileriniz ektedir.", kvkk.ReauthProof{Password: matrixReauthPassword}); err != nil {
		t.Fatal(err)
	}
	drain(t, e.c)
	got, err := e.c.kvkk.svc.GetMine(ctx, subject, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := e.c.kvkk.svc.DownloadMine(ctx, subject, d.ID)
	if err != nil || !strings.Contains(dl.URL, "X-Amz-Expires=300") { // 5 minutes
		t.Fatalf("download = %+v, %v", dl, err)
	}
	row, err := e.c.kvkk.store.Get(ctx, got.ID)
	if err != nil || row.ExportFileID == nil {
		t.Fatalf("request = %+v, %v", row, err)
	}
	f, err := e.c.files.store.Get(ctx, *row.ExportFileID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _, ok := e.c.files.objects.(*storage.Mem).Object(f.Bucket, f.StorageKey)
	if !ok {
		t.Fatal("export object missing")
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, zf := range zr.File {
		names = append(names, zf.Name)
	}
	want := []string{"manifest.json"}
	for _, p := range e.c.privacy {
		want = append(want, p.Name()+".json")
	}
	slices.Sort(want[1:])
	if !slices.Equal(names, want) {
		t.Fatalf("zip = %v, want %v", names, want)
	}
}
