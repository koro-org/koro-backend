//go:build integration

package talent_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/testutil"
	"koro/internal/students"
	"koro/internal/talent"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var userSeq atomic.Int64

// pgUser inserts a user as app_rw (the FK target of every talent row; the
// signup persona does not matter, the students port is faked).
func pgUser(d *db.DB) func(t *testing.T) uuid.UUID {
	return func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.NewV7()
		email := "talent" + strconv.FormatInt(userSeq.Add(1), 10) + "@example.test"
		if _, err := d.Pool.Exec(context.WithoutCancel(t.Context()), `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
			VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, email); err != nil {
			t.Fatal(err)
		}
		return id
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) contractEnv {
		d := testutil.NewDB(t)
		return contractEnv{store: talent.NewGormStore(d.ORM), tx: d.Tx, user: pgUser(d)}
	})
}

// TestTalentStore_UsesAppTalentRole (01 §4.11, 02 §0.6, 05 §15.4): the
// GORM store runs on the app_rw pool but every statement after Enter runs
// as app_talent; without Enter it refuses to run; the role ends with the
// transaction (the pooled connection is app_rw again); inside AsApp the
// transaction is app_rw and the talent tables are out of reach (42501).
func TestTalentStore_UsesAppTalentRole(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	st := talent.NewGormStore(d.ORM)
	current := func(ctx context.Context) string {
		t.Helper()
		var who string
		if err := db.Conn(ctx, d.ORM).Raw(`SELECT current_user`).Scan(&who).Error; err != nil {
			t.Fatal(err)
		}
		return who
	}
	if who := current(t.Context()); who != "app_rw" {
		t.Fatalf("pool role = %s, want app_rw", who)
	}
	if _, err := st.Enter(t.Context()); err == nil {
		t.Fatal("Enter outside a transaction succeeded")
	}
	err := d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		if _, err := st.SignalDef(ctx, talent.SignalFeedLike); err == nil {
			t.Error("store read without Enter")
		}
		if who := current(ctx); who != "app_rw" {
			t.Errorf("before Enter: %s", who)
		}
		tctx, err := st.Enter(ctx)
		if err != nil {
			return err
		}
		if who := current(tctx); who != "app_talent" {
			t.Errorf("after Enter: current_user = %s, want app_talent", who)
		}
		if _, err := st.SignalDef(tctx, talent.SignalFeedLike); err != nil {
			t.Errorf("store read as app_talent: %v", err)
		}
		if err := st.AsApp(tctx, func(actx context.Context) error {
			if who := current(actx); who != "app_rw" {
				t.Errorf("inside AsApp: %s, want app_rw", who)
			}
			if _, err := st.SignalDef(actx, talent.SignalFeedLike); err == nil {
				t.Error("store read inside AsApp")
			}
			var n int
			err := db.Conn(actx, d.ORM).Raw(`SELECT count(*) FROM talent.signal_defs`).Scan(&n).Error
			if db.SQLState(err) != db.CodeInsufficientPriv {
				t.Errorf("talent read on app_rw inside the transaction: %v, want 42501", err)
			}
			return nil
		}); err == nil {
			t.Error("AsApp after a failed statement went on") // the 42501 aborted the transaction
		}
		return errors.New("rollback")
	})
	if err == nil || err.Error() != "rollback" {
		t.Fatalf("transaction: %v", err)
	}
	// A clean transaction: AsApp switches back to app_talent afterwards.
	err = d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		tctx, err := st.Enter(ctx)
		if err != nil {
			return err
		}
		if err := st.AsApp(tctx, func(context.Context) error { return nil }); err != nil {
			return err
		}
		if who := current(tctx); who != "app_talent" {
			t.Errorf("after AsApp: %s, want app_talent", who)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 { // the role never leaks into the pool
		if who := current(t.Context()); who != "app_rw" {
			t.Fatalf("pool role after the transactions = %s, want app_rw", who)
		}
	}
}

// TestRoleBoundaries_AppRwCannotReadTalent (06 H-3, 05 §15.4; the store's
// side of db/migrations' test of the same name): the very pool the talent
// store uses reaches no talent table on its own (42501 for every table and
// statement kind); only the store's SET LOCAL ROLE opens the schema.
func TestRoleBoundaries_AppRwCannotReadTalent(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	user := pgUser(d)(t)
	for _, table := range []string{"config_versions", "signal_defs", "point_events", "daily_counters", "cohorts", "scores",
		"score_runs", "neutralizations"} {
		for _, sql := range []string{
			`SELECT * FROM talent.` + table,
			`DELETE FROM talent.` + table + ` WHERE false`,
			`INSERT INTO talent.` + table + ` DEFAULT VALUES`,
		} {
			_, err := d.Pool.Exec(t.Context(), sql)
			if db.SQLState(err) != db.CodeInsufficientPriv {
				t.Errorf("app_rw %q: %v, want 42501", sql, err)
			}
		}
	}
	if _, err := d.Pool.Exec(t.Context(), `SELECT talent.point_events_erase_user($1)`, user); db.SQLState(err) != db.CodeInsufficientPriv {
		t.Errorf("app_rw calls the ledger erase function: %v, want 42501", err)
	}
	// The store itself reads through its role.
	ce := contractEnv{store: talent.NewGormStore(d.ORM), tx: d.Tx}
	ce.must(t, func(ctx context.Context) error {
		_, err := ce.store.SignalDefs(ctx)
		return err
	})
}

// pgEnv is the module over PostgreSQL with the real audit module and fake
// students/directory ports.
type pgEnv struct {
	*env
	d     *db.DB
	user  func(t *testing.T) uuid.UUID
	ownr  *pgx.Conn // app_owner, sequential use only
	au    talent.AuditLogger
	pgRec *pgDestructions // destruction_logs rows in the talent transaction (BE-087)
}

func newPGEnv(t *testing.T, enabled bool) pgEnv {
	t.Helper()
	return newPGEnvWith(t, envOptions{collection: enabled})
}

// newPGEnvWith is newPGEnv with the auditor API switch and the fake
// re-authentication of the unit tests (o.config is ignored: an empty
// version is always in force since the day before t0).
func newPGEnvWith(t *testing.T, o envOptions) pgEnv {
	t.Helper()
	d := testutil.NewDB(t)
	clk := clock.NewFake(t0)
	au := audit.New(audit.Deps{Store: audit.NewGormStore(d.ORM), Tx: d.Tx, Clock: clk, Log: obs.Discard()})
	e := &env{clk: clk, subs: outbox.NewRegistry(), audit: &fakeAudit{},
		students: &fakeStudents{cohorts: map[uuid.UUID]students.Cohort{}, choices: map[uuid.UUID]students.RankingChoice{}},
		dir:      fakeDirectory{sensitive: map[uuid.UUID]bool{}}, reauth: &audittest.LateReauth[identity.ReauthProof]{Next: fakeReauth{}}}
	e.newPorts()
	st := talent.NewGormStore(d.ORM)
	p := pgEnv{env: e, d: d, user: pgUser(d), ownr: testutil.OwnerConn(t, d), au: au.Service, pgRec: &pgDestructions{orm: d.ORM}}
	e.mod = p.module(t, o)
	e.mod.Subscribe(e.subs)
	creator := p.user(t)
	ce := contractEnv{store: st, tx: d.Tx}
	ce.must(t, func(ctx context.Context) error {
		var err error
		e.cfg, err = st.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: t0.Add(-24 * time.Hour), CreatedBy: creator})
		return err
	})
	return p
}

// module builds a talent module over the env's database with the switches
// of o (o.config is ignored); a second module over the same database is
// the process after a restart with other flags (BE-087).
func (p pgEnv) module(t *testing.T, o envOptions) talent.Module {
	t.Helper()
	mod, err := talent.New(talent.Deps{Store: talent.NewGormStore(p.d.ORM), Tx: p.d.Tx, Audit: p.au, Clock: p.clk,
		Log: obs.Discard(), Students: p.students, Directory: p.dir, Accounts: p.accounts, Events: p.events,
		Memberships: p.members, Likes: p.likes, Destructions: p.pgRec, CollectionEnabled: o.collection, AuditorAPIEnabled: o.auditorAPI,
		Reauth: p.reauth})
	if err != nil {
		t.Fatal(err)
	}
	return mod
}

// pgStudent inserts a user and registers it as an adult student.
func (p pgEnv) pgStudent(t *testing.T) uuid.UUID {
	t.Helper()
	id := p.user(t)
	p.students.mu.Lock()
	defer p.students.mu.Unlock()
	p.students.cohorts[id] = students.Cohort{UniversityID: uuid.NewV7()}
	return id
}

// owner counts rows as the schema owner (outside every application role).
func (p pgEnv) owner(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := p.ownr.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestIngest_GormPipeline: the consumers over PostgreSQL: the flag off
// writes nothing; with the flag on a signal, its duplicate and its
// reversal land once each; an objection is stored and audited in the same
// transaction (the audit row is written on the session role inside the
// talent transaction); Bands reads through app_talent.
func TestIngest_GormPipeline(t *testing.T) {
	t.Parallel()
	off := newPGEnv(t, false)
	u := off.pgStudent(t)
	off.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: u, AuthorOrgID: uuid.NewV7()}, t0)
	if n := off.owner(t, `SELECT count(*) FROM talent.point_events`); n != 0 {
		t.Fatalf("flag off wrote %d ledger rows", n)
	}

	p := newPGEnv(t, true)
	user := p.pgStudent(t)
	like := domainevent.PostLiked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: uuid.NewV7()}
	ev := envelope(t, like, t0)
	p.deliver(t, ev)
	p.deliver(t, ev)
	p.send(t, domainevent.PostUnliked{PostID: like.PostID, UserID: user, AuthorOrgID: like.AuthorOrgID}, t0)
	p.send(t, domainevent.PostUnliked{PostID: like.PostID, UserID: user, AuthorOrgID: like.AuthorOrgID}, t0)
	if n := p.owner(t, `SELECT count(*) FROM talent.point_events WHERE user_id = $1`, user); n != 2 {
		t.Fatalf("ledger rows = %d, want the like and one reversal", n)
	}
	if n := p.owner(t, `SELECT COALESCE(sum(points_cp), 0)::int FROM talent.point_events WHERE user_id = $1`, user); n != 0 {
		t.Fatalf("sum = %d, want 0", n)
	}
	if n := p.owner(t, `SELECT applied_cp FROM talent.daily_counters WHERE user_id = $1`, user); n != 10 {
		t.Fatalf("daily counter = %d, want 10", n)
	}

	dsr := uuid.NewV7()
	p.send(t, domainevent.DSRProfilingObjection{DSRID: dsr, UserID: user}, t0)
	if n := p.owner(t, `SELECT count(*) FROM talent.neutralizations WHERE user_id = $1 AND reason = 'objection' AND dsr_id = $2 AND lifted_at IS NULL`, user, dsr); n != 1 {
		t.Fatalf("objection rows = %d", n)
	}
	if n := p.owner(t, `SELECT count(*) FROM audit_events WHERE action = 'talent.neutralization.manage' AND resource_id = $1 AND system_job = $2`,
		user, talent.NeutralizeObjectionTaskName); n != 1 {
		t.Fatalf("objection audit rows = %d, want 1", n)
	}
	// The subject report is audited too, and Bands reads the score.
	if _, err := p.mod.Auditor.SubjectReport(t.Context(), uuid.Nil(), user, "dsr:"+dsr.String()); err != nil {
		t.Fatal(err)
	}
	if n := p.owner(t, `SELECT count(*) FROM audit_events WHERE action = 'talent.report.read' AND resource_id = $1`, user); n != 1 {
		t.Fatalf("report audit rows = %d, want 1", n)
	}
	other := p.pgStudent(t)
	if _, err := p.ownr.Exec(t.Context(), `INSERT INTO talent.scores (user_id, raw_cp, pct_final, band, n_signals, cohort_key,
		config_version_id, run_id, calculated_at) VALUES ($1, 900, 0.9500, 3, 4, 'global', $3, gen_random_uuid(), now()),
		($2, 900, 0.9500, 3, 4, 'global', $3, gen_random_uuid(), now())`, user, other, p.cfg.ID); err != nil {
		t.Fatal(err)
	}
	bands, err := p.mod.Ranking.Bands(t.Context(), []uuid.UUID{user, other, uuid.NewV7()})
	if err != nil {
		t.Fatal(err)
	}
	if len(bands) != 1 || bands[other] != talent.Band(3) {
		t.Fatalf("bands has %d entries, want only the non-objector", len(bands))
	}
}

// TestIngest_ConcurrentSignals_DailyCapHolds: concurrent consumers of one
// user's signal queue behind the counter row lock: eight likes at once
// apply exactly the 50 cp daily cap, each post once.
func TestIngest_ConcurrentSignals_DailyCapHolds(t *testing.T) {
	t.Parallel()
	p := newPGEnv(t, true)
	user := p.pgStudent(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			ev := envelope(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: uuid.NewV7()}, t0)
			if err := p.subs.For(ev.Topic)[0].Handle(context.WithoutCancel(t.Context()), ev); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := p.owner(t, `SELECT COALESCE(sum(points_cp), 0)::int FROM talent.point_events WHERE user_id = $1`, user); n != 50 {
		t.Fatalf("applied = %d cp, want the 50 cp cap", n)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.point_events WHERE user_id = $1`, user); n != 8 {
		t.Fatalf("rows = %d, want 8 (3 capped)", n)
	}
	if n := p.owner(t, `SELECT applied_cp FROM talent.daily_counters WHERE user_id = $1`, user); n != 50 {
		t.Fatalf("counter = %d", n)
	}
}

// TestPrivacy_GormErase (01 §3.3, 000121): the KVKK provider erases every
// talent row of the subject; the append-only ledger only once the
// subject's erasure request is completed (55000 before, nothing deleted).
func TestPrivacy_GormErase(t *testing.T) {
	t.Parallel()
	p := newPGEnv(t, true)
	user, other := p.pgStudent(t), p.pgStudent(t)
	for _, u := range []uuid.UUID{user, other} {
		p.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: u, AuthorOrgID: uuid.NewV7()}, t0)
		p.send(t, domainevent.DSRProfilingObjection{DSRID: uuid.NewV7(), UserID: u}, t0)
	}
	if _, err := p.mod.Privacy.Erase(t.Context(), user); err == nil {
		t.Fatal("ledger erased without a completed erasure request")
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.neutralizations WHERE user_id = $1`, user); n != 1 {
		t.Fatal("a refused erasure deleted rows (no rollback)")
	}
	if _, err := p.d.Pool.Exec(t.Context(), `INSERT INTO data_subject_requests (user_id, type, channel, status, due_at, completed_at)
		VALUES ($1, 'erasure', 'in_app', 'completed', now() + interval '30 days', now())`, user); err != nil {
		t.Fatal(err)
	}
	res, err := p.mod.Privacy.Erase(t.Context(), user)
	if err != nil || res.Deleted != 3 {
		t.Fatalf("Erase = %+v, %v; want the counter, the neutralisation and the ledger row", res, err)
	}
	for _, table := range []string{"point_events", "daily_counters", "neutralizations"} {
		if n := p.owner(t, `SELECT count(*) FROM talent.`+table+` WHERE user_id = $1`, user); n != 0 {
			t.Errorf("talent.%s keeps %d rows of the erased user", table, n)
		}
		if n := p.owner(t, `SELECT count(*) FROM talent.`+table+` WHERE user_id = $1`, other); n != 1 {
			t.Errorf("talent.%s lost the other user's row", table)
		}
	}
	if res, err := p.mod.Privacy.Erase(t.Context(), user); err != nil || res.Deleted != 0 {
		t.Fatalf("second Erase = %+v, %v", res, err)
	}
	sec, err := p.mod.Privacy.Export(t.Context(), other)
	if err != nil || sec.Items[0]["profiling_data_held"] != true {
		t.Fatalf("export = %+v, %v", sec, err)
	}
}
