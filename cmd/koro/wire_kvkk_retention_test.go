package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/db/migrations"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/kvkk"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/privacy"
)

// Retention (BE-057, kvkk-3): every retention policy has its sweeper (or a
// job, or a reasoned exception), every personal table its retention, and
// the sweepers of the real container destroy nothing before the legal
// approval, record every run and work in batches.

// ---------------------------------------------------------------- seed

var (
	seedTupleRe  = regexp.MustCompile(`\(\s*'([a-z_]+)',\s*'([a-z_]+)',\s*'([^']*)',\s*'([^']*)',\s*'([a-z_]+)',\s*'([a-z_]+)',\s*'((?:[^']|'')*)',\s*(true|false),\s*'((?:[^']|'')*)'\s*\)`)
	seedUpdateRe = regexp.MustCompile(`UPDATE public\.retention_policies SET (retention = interval|anchor =|target =) '([^']+)'\s+WHERE code = '([a-z_]+)'`)
	intervalRe   = regexp.MustCompile(`^(\d+)(?: (years?|months?|days?|hours?))?$`)
)

// seedPolicies reads the retention_policies seed of the migrations
// (000090 and the 000121/000123/000134/000142/000147/000148 additions and updates) as kvkk.Policy rows, none
// legally approved (T-35).
func seedPolicies(t *testing.T) map[string]kvkk.Policy {
	t.Helper()
	out := map[string]kvkk.Policy{}
	for _, name := range []string{"000090_seed_retention_policies.up.sql", "000121_create_retention_purge_and_analytics_views.up.sql", "000123_refund_followups.up.sql",
		"000134_seed_talent_collection_off_policy.up.sql", "000142_seed_talent_score_dropped_policy.up.sql",
		"000147_iys_netgsm_events.up.sql",          // BE-110: the table and its policy
		"000148_create_terms_acceptances.up.sql"} { // BE-118: the terms ledger and its policy
		b, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if i := strings.Index(src, "INSERT INTO "); i >= 0 {
			for _, m := range seedTupleRe.FindAllStringSubmatch(src[i:], -1) {
				p := kvkk.Policy{Code: m[1], OwnerModule: m[2], Target: m[3], Period: parseInterval(t, m[4]), Anchor: m[5], Method: m[6],
					LegalBasis: strings.ReplaceAll(m[7], "''", "'"), Enabled: m[8] == "true", DescriptionTR: m[9]}
				out[p.Code] = p
			}
		}
		for _, m := range seedUpdateRe.FindAllStringSubmatch(src, -1) {
			p, ok := out[m[3]]
			if !ok {
				t.Fatalf("%s updates the unknown policy %s", name, m[3])
			}
			if strings.HasPrefix(m[1], "retention") {
				p.Period = parseInterval(t, m[2])
			} else if strings.HasPrefix(m[1], "target") {
				p.Target = m[2]
			} else {
				p.Anchor = m[2]
			}
			out[m[3]] = p
		}
	}
	if len(out) != 49 { // 02 §24.2 (44) + privileged_action_history (000121) + talent_collection_off (000134) + talent_score_dropped (000142) + iys_netgsm_event (000147) + terms_acceptance (000148); TestSchema_RetentionSeedComplete
		t.Fatalf("read %d seeded retention policies, want 49", len(out))
	}
	return out
}

// parseInterval reads the interval literals of the seed ("7 days", "0").
func parseInterval(t *testing.T, s string) kvkk.Period {
	t.Helper()
	m := intervalRe.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("interval %q", s)
	}
	n, _ := strconv.Atoi(m[1])
	switch strings.TrimSuffix(m[2], "s") {
	case "year":
		return kvkk.Period{Years: n}
	case "month":
		return kvkk.Period{Years: n / 12, Months: n % 12}
	case "day":
		return kvkk.Period{Days: n}
	case "hour":
		return kvkk.Period{Hours: n}
	}
	return kvkk.Period{}
}

// targetTables returns the tables a policy target names: "files:<purpose>"
// is files, "<table>.<column>" the table, a talent table keeps its schema.
func targetTables(target string, schema map[string]bool) []string {
	var out []string
	for part := range strings.SplitSeq(target, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "files:"):
			part = "files"
		default:
			if _, isTable := schema[part]; isTable {
				break
			}
			if i := strings.LastIndexByte(part, '.'); i > 0 {
				part = part[:i]
			}
		}
		if !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	return out
}

// ---------------------------------------------------------------- catalogs

type retentionLine struct {
	key, coverage string
	line          int
}

func readRetentionCatalog(t *testing.T, path string) []retentionLine {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []retentionLine
	for i, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		f := strings.Fields(ln)
		if len(f) < 2 {
			t.Fatalf("%s:%d: want <key> <coverage>", path, i+1)
		}
		out = append(out, retentionLine{key: f[0], coverage: strings.Join(f[1:], " "), line: i + 1})
	}
	return out
}

// reason returns the text after prefix when it is a reason of at least 20
// characters.
func reason(coverage, prefix string) bool {
	r, ok := strings.CutPrefix(coverage, prefix)
	return ok && len([]rune(r)) >= 20
}

// claimedCodes maps every policy code the container's sweepers claim to
// how many sweepers claim it.
func claimedCodes(c *container) map[string]int {
	out := map[string]int{}
	for _, sw := range c.sweepers {
		for _, code := range sw.PolicyCodes() {
			out[code]++
		}
	}
	return out
}

// TestEveryRetentionPolicyHasSweeper (BE-057): every seeded retention
// policy is applied: by a sweeper of its owner module registered in the
// container (run by kvkk.retention_sweep), by a scheduled job, in the
// transaction that ends the record, or it names the ticket that brings
// its code (todo) or the reason it has none (dsr_erasure, dormant_account).
// No sweeper claims a code twice or a code that is not seeded.
func TestEveryRetentionPolicyHasSweeper(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	seed := seedPolicies(t)
	claims := claimedCodes(c)
	jobs := map[string]bool{}
	for _, j := range c.infra.jobs.Jobs() {
		jobs[j.Name] = true
	}
	seen := map[string]bool{}
	swept := 0
	for _, l := range readRetentionCatalog(t, "testdata/kvkk/retention_policies.txt") {
		at := fmt.Sprintf("retention_policies.txt:%d (%s)", l.line, l.key)
		p, ok := seed[l.key]
		if !ok {
			t.Errorf("%s: no such seeded retention policy", at)
			continue
		}
		if seen[l.key] {
			t.Errorf("%s: listed twice", at)
		}
		seen[l.key] = true
		switch {
		case strings.HasPrefix(l.coverage, "sweeper:"):
			swept++
			if mod := strings.TrimPrefix(l.coverage, "sweeper:"); mod != p.OwnerModule {
				t.Errorf("%s: sweeper of %s, but the policy belongs to %s", at, mod, p.OwnerModule)
			}
			if claims[l.key] != 1 {
				t.Errorf("%s: %d registered sweepers claim the policy, want 1", at, claims[l.key])
			}
		case strings.HasPrefix(l.coverage, "job:"):
			if job := strings.TrimPrefix(l.coverage, "job:"); !jobs[job] {
				t.Errorf("%s: job %s is not scheduled", at, job)
			}
		case reason(l.coverage, "inline:"), regexp.MustCompile(`^todo:BE-\d{3}$`).MatchString(l.coverage):
		case reason(l.coverage, "none:"):
			if l.key != kvkk.ErasurePolicyCode && l.key != "dormant_account" {
				t.Errorf("%s: only dsr_erasure and dormant_account go without a sweeper by design", at)
			}
		default:
			t.Errorf("%s: coverage %q (sweeper:<module>, job:<name>, inline:<reason>, todo:BE-NNN or none:<reason>)", at, l.coverage)
		}
		if !strings.HasPrefix(l.coverage, "sweeper:") && claims[l.key] > 0 {
			t.Errorf("%s: a sweeper claims the policy; mark it sweeper:%s", at, p.OwnerModule)
		}
	}
	for code := range seed {
		if !seen[code] {
			t.Errorf("retention policy %s is not in testdata/kvkk/retention_policies.txt", code)
		}
	}
	for code := range claims {
		if _, ok := seed[code]; !ok {
			t.Errorf("a sweeper claims %s, which is not a seeded retention policy", code)
		}
	}
	if swept < 20 {
		t.Errorf("only %d policies have a sweeper", swept)
	}
}

// TestEveryPersonalTableHasRetention (BE-057; KVKK md. 7): every personal
// or special category table of personal_tables.txt says how long its rows
// live: the retention policies whose target names it, the account's life
// (erased by its module's provider), its parent row, or the ticket that
// decides it. Conversely every table a seeded policy names lists that
// policy. Mirrors TestEveryPersonalTableHasProvider (T-92).
func TestEveryPersonalTableHasRetention(t *testing.T) {
	t.Parallel()
	seed := seedPolicies(t)
	schema := schemaTables(t)
	catalog := map[string]catalogLine{}
	for _, l := range readCatalog(t) {
		catalog[l.table] = l
	}
	lines := map[string]retentionLine{}
	for _, l := range readRetentionCatalog(t, "testdata/kvkk/retention_tables.txt") {
		at := fmt.Sprintf("retention_tables.txt:%d (%s)", l.line, l.key)
		if _, dup := lines[l.key]; dup {
			t.Errorf("%s: listed twice", at)
		}
		lines[l.key] = l
		if _, ok := schema[l.key]; !ok {
			t.Errorf("%s: no such table in db/migrations", at)
		}
	}
	policiesOf := func(l retentionLine) []string {
		codes, ok := strings.CutPrefix(l.coverage, "policy:")
		if !ok {
			return nil
		}
		return strings.Split(codes, ",")
	}
	for table, l := range lines {
		at := fmt.Sprintf("retention_tables.txt:%d (%s)", l.line, table)
		switch {
		case strings.HasPrefix(l.coverage, "policy:"):
			for _, code := range policiesOf(l) {
				p, ok := seed[code]
				switch {
				case !ok:
					t.Errorf("%s: no seeded retention policy %q", at, code)
				case !slices.Contains(targetTables(p.Target, schema), table):
					t.Errorf("%s: the target of %s (%q) does not name the table", at, code, p.Target)
				}
			}
		case reason(l.coverage, "account:"):
			cov := catalog[table].coverage
			if !strings.HasPrefix(cov, "provider:") && !strings.HasPrefix(cov, "todo:") {
				t.Errorf("%s: kept for the account's life, but no provider erases it (personal_tables: %q)", at, cov)
			}
		case strings.HasPrefix(l.coverage, "parent:"):
			parent, ok := lines[strings.TrimPrefix(l.coverage, "parent:")]
			if !ok || (!strings.HasPrefix(parent.coverage, "policy:") && !strings.HasPrefix(parent.coverage, "account:")) {
				t.Errorf("%s: the parent has no retention line", at)
			}
		case regexp.MustCompile(`^todo:BE-\d{3}$`).MatchString(l.coverage):
		default:
			t.Errorf("%s: retention %q (policy:<codes>, account:<reason>, parent:<table> or todo:BE-NNN)", at, l.coverage)
		}
	}
	// Every personal and special category table has a line.
	for table, l := range catalog {
		if l.class != "personal" && l.class != "special" {
			continue
		}
		if _, ok := lines[table]; !ok {
			t.Errorf("personal table %s (%s) has no line in testdata/kvkk/retention_tables.txt", table, l.owner)
		}
	}
	// Every table a policy names lists the policy.
	for code, p := range seed {
		for _, table := range targetTables(p.Target, schema) {
			if _, ok := schema[table]; !ok {
				t.Errorf("policy %s names %q, which is not a table", code, table)
				continue
			}
			if !slices.Contains(policiesOf(lines[table]), code) {
				t.Errorf("policy %s names %s, but retention_tables.txt does not list it for the table", code, table)
			}
		}
	}
}

// ---------------------------------------------------------------- runs

// retentionEnv is the real container (in-memory stores) with the full
// retention seed, unapproved, and the eligible rows of four sweepers.
type retentionEnv struct {
	c       *container
	clk     *clock.Fake
	records *kvkk.MemRecords
	now     time.Time

	inApp       uuid.UUID // notify: 7 months old (in_app_notification, 6 months)
	club        uuid.UUID
	member      uuid.UUID // orgs: ended 7 months ago (membership_ended, 6 months)
	consentUser uuid.UUID // consent: withdrawn 4 years ago (consent_proof, 3 years)
	breach      uuid.UUID // kvkk: closed 6 years ago (breach_record, 5 years)
}

func newRetentionEnv(t *testing.T) *retentionEnv {
	t.Helper()
	c := newTestContainer(t)
	records := kvkk.NewMemRecords(c.infra.clock.Now)
	for _, p := range seedPolicies(t) {
		p.CreatedAt, p.UpdatedAt = matrixRecordsAt, matrixRecordsAt
		records.PutPolicy(p)
	}
	c.kvkk.records = records
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	e := &retentionEnv{c: c, clk: c.infra.clock.(*clock.Fake), records: records}
	e.now = e.clk.Now()
	ctx := t.Context()

	user := uuid.NewV7()
	e.inApp = uuid.NewV7()
	if _, err := c.notify.store.InsertInApp(ctx, notify.InAppNotification{ID: e.inApp, UserID: user, Kind: "notify.test_message",
		Title: "Eski bildirim", CreatedAt: e.now.AddDate(0, -7, 0)}); err != nil {
		t.Fatal(err)
	}

	club, _, err := c.orgs.store.InsertOrg(ctx, orgs.Org{ID: uuid.NewV7(), Kind: orgs.KindClub, Name: "Saklama Kulübü",
		Slug: "saklama-" + uuid.NewV7().String()[:8], Status: orgs.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	e.club = club.ID
	joined := e.now.AddDate(-1, 0, 0)
	m, err := c.orgs.store.InsertMember(ctx, orgs.Membership{ID: uuid.NewV7(), OrgID: club.ID, UserID: user, Role: authz.MemberMember,
		Status: orgs.MemberActive, Source: orgs.SourceAdmin, JoinedAt: &joined})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.orgs.store.EndMember(ctx, club.ID, m.ID, e.now.AddDate(0, -7, 0), orgs.EndLeft); err != nil {
		t.Fatal(err)
	}
	e.member = m.ID

	e.consentUser = uuid.NewV7()
	if err := c.consent.store.InsertConsent(ctx, consent.Consent{ID: uuid.NewV7(), UserID: e.consentUser,
		PurposeCode: consent.PurposeCommercialEmail, Action: consent.Withdrawn, ScopeType: consent.ScopeNone,
		Channel: consent.ChannelWeb, OccurredAt: e.now.AddDate(-4, 0, 0)}); err != nil {
		t.Fatal(err)
	}

	closed := e.now.AddDate(-6, 0, 0)
	e.breach = uuid.NewV7()
	records.PutBreach(kvkk.Breach{ID: e.breach, Title: "Eski ihlal", DetectedAt: closed, AuthorityDeadlineAt: closed.Add(kvkk.AuthorityPeriod),
		Status: kvkk.BreachClosed, ClosedAt: &closed, OwnerUserID: uuid.NewV7(), CreatedAt: closed, UpdatedAt: closed})
	return e
}

// runSweep runs kvkk.retention_sweep once.
func (e *retentionEnv) runSweep(t *testing.T) {
	t.Helper()
	for _, j := range e.c.infra.jobs.Jobs() {
		if j.Name == kvkk.RetentionSweepJobName {
			if err := j.Handle(workerContext(t)); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("kvkk.retention_sweep is not scheduled")
}

// present reports which of the four eligible records still exist.
func (e *retentionEnv) present(t *testing.T) map[string]bool {
	t.Helper()
	ctx := t.Context()
	n, err := e.c.notify.store.PurgeExpired(ctx, notify.PolicyInAppNotification, e.now.AddDate(0, -6, 0), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	_, merr := e.c.orgs.store.GetMember(ctx, e.club, e.member)
	cs, err := e.c.consent.store.AllOf(ctx, e.consentUser)
	if err != nil {
		t.Fatal(err)
	}
	_, berr := e.records.Breach(ctx, e.breach)
	return map[string]bool{"notify": n == 1, "orgs": merr == nil, "consent": len(cs) == 1, "kvkk": berr == nil}
}

// approveClaimed marks every policy a registered sweeper claims as legally
// approved (the lawyer's signature, T-35).
func (e *retentionEnv) approveClaimed(t *testing.T) {
	t.Helper()
	for code := range claimedCodes(e.c) {
		p, err := e.records.Policy(t.Context(), code)
		if err != nil {
			t.Fatal(err)
		}
		p.LegalApproved, p.ApprovedRef = true, "Hukuk görüşü 2026/21"
		e.records.PutPolicy(p)
	}
}

// logsByPolicy groups the destruction rows of the policies by code,
// newest first.
func (e *retentionEnv) logsByPolicy(t *testing.T) map[string][]kvkk.DestructionLog {
	t.Helper()
	rows, err := e.records.DestructionLogs(t.Context(), kvkk.DestructionFilter{}, nil, 10000)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]kvkk.DestructionLog{}
	for _, l := range rows {
		out[l.PolicyCode] = append(out[l.PolicyCode], l)
	}
	return out
}

// TestSweeper_DryRunWhenNotLegallyApproved (T-35; BE-057): with the seed
// as migrated (no policy legally approved), the nightly run of every
// registered sweeper destroys nothing: the eligible rows of notify, orgs,
// consent and kvkk stay, and each run is recorded as a dry run with what
// it would have destroyed. After the approval the same run destroys them.
func TestSweeper_DryRunWhenNotLegallyApproved(t *testing.T) {
	t.Parallel()
	e := newRetentionEnv(t)
	want := map[string]bool{"notify": true, "orgs": true, "consent": true, "kvkk": true}
	if got := e.present(t); !mapsEqual(got, want) {
		t.Fatalf("fixture = %v", got)
	}
	e.runSweep(t)
	if got := e.present(t); !mapsEqual(got, want) {
		t.Fatalf("an unapproved sweep destroyed: present = %v", got)
	}
	logs := e.logsByPolicy(t)
	for code := range claimedCodes(e.c) {
		rows := logs[code]
		if len(rows) != 1 || rows[0].RecordCount != 0 || rows[0].Details["dry_run"] != true {
			t.Errorf("%s: destruction rows %+v, want one dry run", code, rows)
		}
	}
	for code, min := range map[string]int{notify.PolicyInAppNotification: 1, orgs.PolicyMembershipEnded: 1,
		consent.PolicyConsentProof: 1, kvkk.PolicyBreachRecord: 1} {
		if rows := logs[code]; len(rows) == 0 || rows[0].Details["eligible"] != min {
			t.Errorf("%s: dry run eligible = %v, want %d", code, rows, min)
		}
	}

	e.approveClaimed(t)
	e.runSweep(t)
	if got := e.present(t); !mapsEqual(got, map[string]bool{"notify": false, "orgs": false, "consent": false, "kvkk": false}) {
		t.Fatalf("after the approval present = %v; want every eligible row destroyed", got)
	}
}

// TestDestructionLog_WrittenPerSweep (02 §16.3, İmha Yönetmeliği md. 7;
// BE-057): every run writes exactly one destruction_logs row per enabled
// policy a sweeper claims (dry or not, failed or not), with the module,
// the target, the method, the cut-off of the period and the count; an
// approved destruction is audited.
func TestDestructionLog_WrittenPerSweep(t *testing.T) {
	t.Parallel()
	e := newRetentionEnv(t)
	e.approveClaimed(t)
	e.runSweep(t)
	e.runSweep(t)
	logs := e.logsByPolicy(t)
	claims := claimedCodes(e.c)
	for code := range claims {
		p, err := e.records.Policy(t.Context(), code)
		if err != nil {
			t.Fatal(err)
		}
		rows := logs[code]
		if !p.Enabled {
			if len(rows) != 0 {
				t.Errorf("%s: a disabled policy was run", code)
			}
			continue
		}
		if len(rows) != 2 {
			t.Errorf("%s: %d destruction rows after two runs, want 2", code, len(rows))
			continue
		}
		for _, l := range rows {
			if l.Module != p.OwnerModule || l.Target == "" || l.Method == "" || l.RunID == uuid.Nil() ||
				!l.PeriodCutoff.Equal(p.Period.Before(e.now)) || l.Details["dry_run"] != false || l.Details["error"] != nil {
				t.Errorf("%s: destruction row %+v", code, l)
			}
		}
	}
	for code, n := range map[string]int{notify.PolicyInAppNotification: 1, orgs.PolicyMembershipEnded: 1, kvkk.PolicyBreachRecord: 1} {
		if rows := logs[code]; len(rows) != 2 || rows[1].RecordCount != n || rows[0].RecordCount != 0 {
			t.Errorf("%s: counts %v, want %d then 0", code, rows, n)
		}
	}
	if rows := logs[consent.PolicyConsentProof]; len(rows) != 2 || rows[1].RecordCount != 1 {
		t.Errorf("consent_proof: rows %v", rows)
	}
	// Policies without a sweeper are never in the record of the run.
	for code := range logs {
		if claims[code] == 0 {
			t.Errorf("%s: destruction rows without a sweeper", code)
		}
	}
}

// countingTx counts the outermost transactions (db.NopTx otherwise).
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

// sweeperOf returns the registered sweeper that claims code.
func sweeperOf(t *testing.T, c *container, code string) privacy.RetentionSweeper {
	t.Helper()
	for _, sw := range c.sweepers {
		if slices.Contains(sw.PolicyCodes(), code) {
			return sw
		}
	}
	t.Fatalf("no sweeper claims %s", code)
	return nil
}

// TestSweepers_BatchesOf1000 (01 §7: ≤ 1000 rows per transaction): with
// 2500 expired rows the approved sweeps of notify, orgs and kvkk delete
// them in three transactions each (1000 + 1000 + 500), so an interrupted
// run keeps what it committed and the next run continues.
func TestSweepers_BatchesOf1000(t *testing.T) {
	t.Parallel()
	const rows = 2500
	for _, tc := range []struct {
		code string
		seed func(t *testing.T, c *container, at time.Time)
		tx   func(c *container, tx *countingTx)
	}{
		{notify.PolicyInAppNotification, func(t *testing.T, c *container, at time.Time) {
			for i := range rows {
				if _, err := c.notify.store.InsertInApp(t.Context(), notify.InAppNotification{ID: uuid.NewV7(), UserID: uuid.NewV7(),
					Kind: "notify.test_message", Title: "Bildirim", CreatedAt: at.Add(-time.Duration(i) * time.Minute)}); err != nil {
					t.Fatal(err)
				}
			}
		}, func(c *container, tx *countingTx) { c.notify.tx = tx }},
		{orgs.PolicyMembershipEnded, func(t *testing.T, c *container, at time.Time) {
			club, _, err := c.orgs.store.InsertOrg(t.Context(), orgs.Org{ID: uuid.NewV7(), Kind: orgs.KindClub, Name: "Toplu Kulüp",
				Slug: "toplu-" + uuid.NewV7().String()[:8], Status: orgs.StatusActive})
			if err != nil {
				t.Fatal(err)
			}
			for i := range rows {
				joined := at.AddDate(-1, 0, 0)
				m, err := c.orgs.store.InsertMember(t.Context(), orgs.Membership{ID: uuid.NewV7(), OrgID: club.ID, UserID: uuid.NewV7(),
					Role: authz.MemberMember, Status: orgs.MemberActive, Source: orgs.SourceAdmin, JoinedAt: &joined})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := c.orgs.store.EndMember(t.Context(), club.ID, m.ID, at.Add(-time.Duration(i)*time.Minute), orgs.EndLeft); err != nil {
					t.Fatal(err)
				}
			}
		}, func(c *container, tx *countingTx) { c.orgs.tx = tx }},
		{kvkk.PolicyBreachRecord, func(t *testing.T, c *container, at time.Time) {
			records := c.kvkk.records.(*kvkk.MemRecords)
			for i := range rows {
				closed := at.Add(-time.Duration(i) * time.Minute)
				records.PutBreach(kvkk.Breach{ID: uuid.NewV7(), Title: "İhlal", DetectedAt: closed, AuthorityDeadlineAt: closed.Add(kvkk.AuthorityPeriod),
					Status: kvkk.BreachClosed, ClosedAt: &closed, OwnerUserID: uuid.NewV7(), CreatedAt: closed, UpdatedAt: closed})
			}
		}, func(c *container, tx *countingTx) { c.kvkk.tx = tx }},
	} {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			c := newTestContainer(t)
			tx := &countingTx{}
			tc.tx(c, tx)
			if err := c.build(); err != nil {
				t.Fatal(err)
			}
			now := c.infra.clock.Now()
			period := now.Sub(now.AddDate(-10, 0, 0)) // every seeded row is older
			tc.seed(t, c, now.Add(-period-time.Hour))
			sw := sweeperOf(t, c, tc.code)
			tx.n = 0
			dry, err := sw.Sweep(t.Context(), privacy.RetentionPolicy{Code: tc.code, Retention: period}, now)
			if err != nil || dry.Count != rows || tx.n != 0 {
				t.Fatalf("dry run = %+v, %v in %d transactions", dry, err, tx.n)
			}
			res, err := sw.Sweep(t.Context(), privacy.RetentionPolicy{Code: tc.code, Retention: period, LegalApproved: true}, now)
			if err != nil || res.Count != rows || tx.n != 3 {
				t.Fatalf("sweep = %+v, %v in %d transactions; want %d rows in 3", res, err, tx.n, rows)
			}
			if again, err := sw.Sweep(t.Context(), privacy.RetentionPolicy{Code: tc.code, Retention: period}, now); err != nil || again.Count != 0 {
				t.Fatalf("rows left = %+v, %v", again, err)
			}
		})
	}
}

// TestRetention_ImmediatePurgesNotDeferred (T-83, 06 H-48; BE-057): the two
// destructions that never wait for the lawyer's approval run with the seed
// as migrated (nothing approved): a sensitive club's membership is deleted
// when it ends, and when a club becomes sensitive its ended memberships go
// at once; an export file is deleted 7 days after the answer and recorded.
func TestRetention_ImmediatePurgesNotDeferred(t *testing.T) {
	t.Parallel()
	e := newRetentionEnv(t)
	ctx := t.Context()
	c := e.c

	// A sensitive club: leaving deletes the membership in the same
	// transaction.
	sensitive, _, err := c.orgs.store.InsertOrg(ctx, orgs.Org{ID: uuid.NewV7(), Kind: orgs.KindClub, Name: "Hassas Kulüp",
		Slug: "hassas-" + uuid.NewV7().String()[:8], Status: orgs.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	c.hooks.clubInfo = func(_ context.Context, id uuid.UUID) (hookClub, bool, error) {
		return hookClub{UniversityID: uuid.NewV7(), JoinPolicy: "approval", Sensitive: id == sensitive.ID}, true, nil
	}
	member := uuid.NewV7()
	joined := e.now.AddDate(0, -1, 0)
	m, err := c.orgs.store.InsertMember(ctx, orgs.Membership{ID: uuid.NewV7(), OrgID: sensitive.ID, UserID: member,
		Role: authz.MemberMember, Status: orgs.MemberActive, Source: orgs.SourceAdmin, JoinedAt: &joined})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.orgs.svc.LeaveMembership(ctx, authz.Actor{Kind: authz.KindUser, UserID: member, SessionID: uuid.NewV7()}, m.ID); err != nil {
		t.Fatalf("LeaveMembership: %v", err)
	}
	if _, err := c.orgs.store.GetMember(ctx, sensitive.ID, m.ID); err == nil {
		t.Fatal("the sensitive club's membership survived its end (membership_ended_sensitive = 0)")
	}

	// A club that becomes sensitive loses its kept ended memberships at
	// once (club.sensitivity_changed → orgs.purge_sensitive_memberships).
	if err := (db.NopTx{}).WithTx(ctx, func(ctx context.Context) error {
		return c.infra.outbox.Add(ctx, mustEnvelope(t, domainevent.ClubSensitivityChanged{ClubOrgID: e.club, Sensitive: true}, e.club))
	}); err != nil {
		t.Fatal(err)
	}
	drain(t, c)
	if _, err := c.orgs.store.GetMember(ctx, e.club, e.member); err == nil {
		t.Fatal("an ended membership of a club that became sensitive survived")
	}
}

// TestRetention_ExportFileGoneAfter7Days (T-83, 06 H-48; BE-057): the
// data export of a completed request is deleted by kvkk.purge_exports 7
// days after the answer although dsr_export_file is not legally approved,
// and the deletion is recorded in destruction_logs (one row per file).
func TestRetention_ExportFileGoneAfter7Days(t *testing.T) {
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
	row, err := e.c.kvkk.store.Get(ctx, d.ID)
	if err != nil || row.ExportFileID == nil {
		t.Fatalf("request = %+v, %v", row, err)
	}
	file := *row.ExportFileID
	if p, err := e.c.kvkk.records.Policy(ctx, kvkk.ExportPolicyCode); err != nil || p.LegalApproved {
		t.Fatalf("dsr_export_file = %+v, %v; the test needs it unapproved", p, err)
	}
	purge := func() {
		t.Helper()
		for _, j := range e.c.infra.jobs.Jobs() {
			if j.Name == kvkk.PurgeExportsJobName {
				if err := j.Handle(workerContext(t)); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatal("kvkk.purge_exports is not scheduled")
	}
	clk := e.c.infra.clock.(*clock.Fake)
	clk.Advance(6 * 24 * time.Hour)
	purge()
	if _, err := e.c.files.store.Get(ctx, file); err != nil {
		t.Fatalf("the export went before its 7 days: %v", err)
	}
	clk.Advance(24*time.Hour + time.Minute)
	purge()
	if _, err := e.c.files.store.Get(ctx, file); err == nil {
		t.Fatal("the export file survived 7 days")
	}
	if row, _ := e.c.kvkk.store.Get(ctx, d.ID); row.ExportFileID != nil {
		t.Fatalf("the request still points at the export: %+v", row)
	}
	logs, err := e.c.kvkk.records.DestructionLogs(ctx, kvkk.DestructionFilter{PolicyCode: kvkk.ExportPolicyCode}, nil, 10)
	if err != nil || len(logs) != 1 || logs[0].RecordCount != 1 || logs[0].Method != privacy.MethodFileDelete || logs[0].Module != "kvkk" {
		t.Fatalf("destruction rows of the export = %+v, %v", logs, err)
	}
}

// mustEnvelope wraps p for the outbox.
func mustEnvelope(t *testing.T, p domainevent.Payload, aggregate uuid.UUID) domainevent.Envelope {
	t.Helper()
	env, err := domainevent.New(p, "club", aggregate)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
