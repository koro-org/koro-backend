//go:build integration

package audit_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"koro/internal/audit"
	"koro/internal/authz/authztest"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

func TestGormStore_Contract(t *testing.T) {
	t.Parallel()
	runStoreContract(t, func(t *testing.T) (audit.Store, db.TxManager) {
		d := testutil.NewDB(t)
		return audit.NewGormStore(d.ORM), d.Tx
	})
}

// pgEnv is an audit module over PostgreSQL (app_rw) with a fake clock.
type pgEnv struct {
	*env
	d *db.DB
}

func newPGEnv(t *testing.T, d *db.DB) *pgEnv {
	t.Helper()
	if d == nil {
		d = testutil.NewDB(t)
	}
	e := newEnv(t, func(dep *audit.Deps) { dep.Store, dep.Tx = audit.NewGormStore(d.ORM), d.Tx })
	return &pgEnv{env: e, d: d}
}

func (e *pgEnv) count(ctx context.Context, t *testing.T, where string) int {
	t.Helper()
	var n int
	if err := e.d.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE `+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Log writes in the caller's transaction: a rolled back business change
// leaves no audit row, a committed one leaves exactly one (01 §4.1).
func TestLogger_SameTxRollback_NoAuditRow(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t, nil)
	ctx := t.Context()
	errBusiness := errors.New("business rule failed after the audit row")
	user := authztest.ID("club_admin")

	err := e.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := e.svc.Log(ctx, audit.Event{Action: "events.event.create", Outcome: audit.Success, ActorUserID: &user}); err != nil {
			return err
		}
		if n := e.count(ctx, t, `true`); n != 0 {
			t.Errorf("uncommitted audit row visible outside the transaction (%d)", n)
		}
		return errBusiness
	})
	if !errors.Is(err, errBusiness) {
		t.Fatalf("WithTx() = %v", err)
	}
	if n := e.count(ctx, t, `true`); n != 0 {
		t.Fatalf("%d audit rows after rollback, want 0", n)
	}

	func() {
		defer func() { _ = recover() }()
		_ = e.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			_ = e.svc.Log(ctx, audit.Event{Action: "events.event.create", Outcome: audit.Success, ActorUserID: &user})
			panic("handler bug")
		})
	}()
	if n := e.count(ctx, t, `true`); n != 0 {
		t.Fatalf("%d audit rows after a panic, want 0", n)
	}

	err = e.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return e.svc.Log(ctx, audit.Event{Action: "events.event.create", Outcome: audit.Success, ActorUserID: &user})
	})
	if err != nil || e.count(ctx, t, `action = 'events.event.create'`) != 1 {
		t.Fatalf("committed: %v, rows %d; want one", err, e.count(ctx, t, `true`))
	}
	// Without a transaction the row is written on its own.
	if err := e.svc.Log(ctx, audit.Event{Action: "identity.session.create", Outcome: audit.Success, ActorUserID: &user}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(ctx, t, `true`); n != 2 {
		t.Fatalf("%d rows, want 2", n)
	}
}

// Writers commit in any order (some hold their transaction open while the
// chain runs) and several chain processes run at once: every row is chained
// exactly once, chain_seq has no gap, and every link points at its single
// predecessor (no fork).
func TestChain_ConcurrentWriters_NoGapNoFork(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	const writers, perWriter, chainers = 6, 40, 3

	// Each chain "process" has its own pool and module, like worker replicas.
	var mods []audit.Module
	for range chainers {
		pool, err := db.Open(ctx, db.Config{URL: testutil.AppURL(t, d), AppName: "koro-test-chainer", MaxConns: 2,
			StatementTimeout: 15 * time.Second, IdleInTxTimeout: 30 * time.Second}, obs.Discard())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		mods = append(mods, audit.New(audit.Deps{Store: audit.NewGormStore(pool.ORM), Tx: pool.Tx, Clock: clock.Real(), Log: obs.Discard()}))
	}
	writerPool, err := db.Open(ctx, db.Config{URL: testutil.AppURL(t, d), AppName: "koro-test-writer", MaxConns: writers + 2,
		StatementTimeout: 15 * time.Second, IdleInTxTimeout: 30 * time.Second}, obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writerPool.Close)
	logger := audit.New(audit.Deps{Store: audit.NewGormStore(writerPool.ORM), Tx: writerPool.Tx, Clock: clock.Real(), Log: obs.Discard()}).Service

	var done atomic.Bool
	var wg sync.WaitGroup
	errs := make(chan error, writers+chainers)
	for c := range chainers {
		wg.Go(func() {
			for !done.Load() {
				if _, err := audit.Chain(ctx, mods[c]); err != nil {
					errs <- fmt.Errorf("chainer %d: %w", c, err)
					return
				}
			}
		})
	}
	var writersWG sync.WaitGroup
	for w := range writers {
		writersWG.Go(func() {
			user := uuid.NewV7()
			for i := range perWriter {
				err := writerPool.Tx.WithTx(ctx, func(ctx context.Context) error {
					if err := logger.Log(ctx, audit.Event{Action: "events.event.update", Outcome: audit.Success, ActorUserID: &user,
						Metadata: map[string]any{"writer": w, "i": i}}); err != nil {
						return err
					}
					if i%10 == w%10 { // a long transaction: commits after later rows were chained
						time.Sleep(30 * time.Millisecond)
					}
					return nil
				})
				if err != nil {
					errs <- fmt.Errorf("writer %d: %w", w, err)
					return
				}
			}
		})
	}
	writersWG.Wait()
	done.Store(true)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if _, err := audit.Chain(ctx, mods[0]); err != nil { // the last rows committed after the final pass
		t.Fatal(err)
	}

	const total = writers * perWriter
	var rows, chained, minSeq, maxSeq, distinctPrev int
	err = d.Pool.QueryRow(ctx, `SELECT count(*), count(chain_seq), coalesce(min(chain_seq), 0), coalesce(max(chain_seq), 0),
		count(DISTINCT prev_hash) FROM audit_events`).Scan(&rows, &chained, &minSeq, &maxSeq, &distinctPrev)
	if err != nil {
		t.Fatal(err)
	}
	if rows != total || chained != total || minSeq != 1 || maxSeq != total || distinctPrev != total {
		t.Fatalf("rows %d, chained %d, chain_seq %d..%d, distinct prev_hash %d; want %d rows chained 1..%d without fork",
			rows, chained, minSeq, maxSeq, distinctPrev, total, total)
	}
	res, err := audit.VerifyRange(ctx, mods[1], day0.AddDays(-1), day0.AddDays(30))
	if err != nil || !res.OK || res.CheckedRows != total {
		t.Fatalf("VerifyRange() = %+v, %v", res, err)
	}
}

// T-45: the chain job runs as audit_chainer, which may write only the four
// chain columns of unchained rows; app_rw may write none of them.
func TestRoleBoundaries_AuditChainerOnlyChainColumns(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t, nil)
	ctx := t.Context()
	user := authztest.ID("student")
	e.logN(t, ctx, 2, "identity.session.create", user, nil)
	if n := e.chain(t); n != 2 {
		t.Fatalf("Chain() as audit_chainer linked %d rows", n)
	}
	var who string
	if err := e.d.Pool.QueryRow(ctx, `SELECT current_user`).Scan(&who); err != nil || who != "app_rw" {
		t.Fatalf("pool role after the chain job = %q, %v; SET LOCAL must not leak", who, err)
	}
	e.logN(t, ctx, 1, "identity.session.create", user, nil)

	state := func(err error) string { return db.SQLState(err) }
	store := audit.NewGormStore(e.d.ORM)
	try := func(sql string, args ...any) string {
		t.Helper()
		var got string
		_ = e.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			if _, err := store.BeginChain(ctx); err != nil {
				t.Fatalf("BeginChain: %v", err)
			}
			got = state(db.Conn(ctx, e.d.ORM).Exec(sql, args...).Error)
			return errors.New("rollback")
		})
		return got
	}
	hash := make([]byte, 32)
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"non-chain column":           {`UPDATE audit_events SET action = 'identity.session.delete' WHERE chain_seq IS NULL`, nil, db.CodeInsufficientPriv},
		"metadata":                   {`UPDATE audit_events SET metadata = '{}' WHERE chain_seq IS NULL`, nil, db.CodeInsufficientPriv},
		"re-chain a chained row":     {`UPDATE audit_events SET row_hash = ? WHERE chain_seq = 1`, []any{hash}, db.CodeRestrictViolation},
		"insert":                     {`INSERT INTO audit_events (actor_kind, system_job, action, outcome) VALUES ('system', 'x', 'a.b.c', 'success')`, nil, db.CodeInsufficientPriv},
		"delete":                     {`DELETE FROM audit_events WHERE chain_seq IS NULL`, nil, db.CodeInsufficientPriv},
		"daily roots":                {`INSERT INTO audit_daily_roots (day, last_chain_seq, root_hash, signature, key_id, external_ref) VALUES ('2020-01-01', 1, ?, '\x00', 'k', 'r')`, []any{hash}, db.CodeInsufficientPriv},
		"chain columns of a new row": {`UPDATE audit_events SET chain_seq = 3, prev_hash = ?, row_hash = ?, chained_at = now() WHERE chain_seq IS NULL`, []any{hash, hash}, ""},
	} {
		if got := try(c.sql, c.args...); got != c.want {
			t.Errorf("audit_chainer %s: SQLSTATE %q, want %q", name, got, c.want)
		}
	}
	// app_rw itself (no SET ROLE) cannot touch the chain columns.
	if got := state(e.d.ORM.WithContext(ctx).Exec(`UPDATE audit_events SET chain_seq = 3, prev_hash = ?, row_hash = ?, chained_at = now() WHERE chain_seq IS NULL`, hash, hash).Error); got != db.CodeInsufficientPriv {
		t.Errorf("app_rw chain update: SQLSTATE %q, want %s", got, db.CodeInsufficientPriv)
	}
	// BeginChain refuses to switch roles outside a transaction.
	if _, err := store.BeginChain(ctx); err == nil {
		t.Error("BeginChain outside a transaction succeeded")
	}
}

// tamperPG builds 10 chained rows with a signed root of day0 and returns an
// owner connection with audit_events_guard disabled: the strongest
// attacker short of replacing the signing key.
func tamperPG(t *testing.T) (*pgEnv, *pgx.Conn) {
	t.Helper()
	e := newPGEnv(t, nil)
	ctx := t.Context()
	e.logN(t, ctx, 10, "identity.session.create", authztest.ID("student"), nil)
	e.chain(t)
	e.clk.Set(t0.Add(24 * time.Hour))
	if err := audit.DailyRoots(ctx, e.m); err != nil {
		t.Fatal(err)
	}
	if res, err := audit.VerifyRange(ctx, e.m, day0, day0); err != nil || !res.OK || res.CheckedRows != 10 {
		t.Fatalf("intact: %+v, %v", res, err)
	}
	owner := testutil.OwnerConn(t, e.d)
	if _, err := owner.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_guard`); err != nil {
		t.Fatal(err)
	}
	return e, owner
}

// The tamper scenarios of TestVerifyChain_DetectsTamper against the real
// table: deleting a row, editing a column, inserting a forged row.
func TestVerifyChain_DetectsTamperInPostgres(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, e *pgEnv, wantSeq int64, problem string) {
		t.Helper()
		res, err := audit.VerifyRange(t.Context(), e.m, day0, day0)
		if err != nil {
			t.Fatal(err)
		}
		if res.OK || *res.FirstBadChainSeq != wantSeq || res.Problem != problem {
			t.Fatalf("Verify = %+v (first bad %v); want %d %s", res, res.FirstBadChainSeq, wantSeq, problem)
		}
	}
	t.Run("deleted row", func(t *testing.T) {
		t.Parallel()
		e, owner := tamperPG(t)
		if _, err := owner.Exec(t.Context(), `DELETE FROM audit_events WHERE chain_seq = 5`); err != nil {
			t.Fatal(err)
		}
		check(t, e, 5, "gap")
	})
	t.Run("edited row", func(t *testing.T) {
		t.Parallel()
		e, owner := tamperPG(t)
		if _, err := owner.Exec(t.Context(), `UPDATE audit_events SET outcome = 'denied' WHERE chain_seq = 7`); err != nil {
			t.Fatal(err)
		}
		check(t, e, 7, "row_hash")
	})
	t.Run("edited ip", func(t *testing.T) {
		t.Parallel()
		e, owner := tamperPG(t)
		if _, err := owner.Exec(t.Context(), `UPDATE audit_events SET ip = '10.0.0.1' WHERE chain_seq = 2`); err != nil {
			t.Fatal(err)
		}
		check(t, e, 2, "row_hash")
	})
	t.Run("inserted row", func(t *testing.T) {
		t.Parallel()
		e, owner := tamperPG(t)
		ctx := t.Context()
		// Make room at chain_seq 3 (two steps: chain_seq is unique).
		for _, sql := range []string{
			`UPDATE audit_events SET chain_seq = -(chain_seq + 1) WHERE chain_seq >= 3`,
			`UPDATE audit_events SET chain_seq = -chain_seq WHERE chain_seq < 0`,
		} {
			if _, err := owner.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
		}
		forged := uuid.NewV7()
		if _, err := owner.Exec(ctx, `INSERT INTO audit_events (id, occurred_at, actor_kind, system_job, action, outcome)
			VALUES ($1, $2, 'system', 'forged', 'kvkk.dsr.approve', 'success')`, forged, t0); err != nil {
			t.Fatal(err)
		}
		rows, err := e.st.Chained(ctx, 1, 1) // row 2
		if err != nil || len(rows) != 1 {
			t.Fatal(rows, err)
		}
		prev := rows[0].RowHash
		un, err := e.st.Unchained(ctx, 1)
		if err != nil || len(un) != 1 || un[0].ID != forged {
			t.Fatal(un, err)
		}
		h, _ := audit.RowHash(prev, un[0]) // a perfectly linked forgery
		if _, err := owner.Exec(ctx, `UPDATE audit_events SET chain_seq = 3, prev_hash = $2, row_hash = $3, chained_at = $4 WHERE id = $1`,
			forged, prev, h, t0); err != nil {
			t.Fatal(err)
		}
		check(t, e, 4, "prev_hash") // the next row still points at the old row 3
	})
}
