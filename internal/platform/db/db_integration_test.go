//go:build integration

package db_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// ownerExec runs DDL as app_owner in d's database.
func ownerExec(t *testing.T, d *db.DB, sql string) {
	t.Helper()
	if _, err := testutil.OwnerConn(t, d).Exec(t.Context(), sql); err != nil {
		t.Fatalf("owner exec: %v", err)
	}
}

// kv is a scratch table for transaction tests.
type kv struct {
	ID  uuid.UUID `gorm:"type:uuid;primaryKey"`
	Val string
}

func (kv) TableName() string { return "kv" }

func newKV(t *testing.T) *db.DB {
	t.Helper()
	d := testutil.NewDB(t)
	ownerExec(t, d, `CREATE TABLE kv (id uuid PRIMARY KEY, val text NOT NULL);
		GRANT SELECT, INSERT, UPDATE, DELETE ON kv TO app_rw`)
	return d
}

func count(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM kv`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func insert(ctx context.Context, d *db.DB, val string) error {
	return db.Conn(ctx, d.ORM).Create(&kv{ID: uuid.NewV7(), Val: val}).Error
}

// TestExecModeTypes shows that the Go types used by stores bind correctly in
// pgx.QueryExecModeExec through GORM and raw pgx (05 §6.1, §18.2-1). Findings
// (BE-002): uuid.UUID, []uuid.UUID (IN ?), json.RawMessage, []byte,
// time.Time, *int, bool, int64 work as they are; text[] and uuid[] need
// db.TextArray / db.UUIDArray because GORM expands a plain slice into a row
// expression; a parameter written directly after "(" ("VALUES (?", "(@id")
// is expanded element-wise by GORM, which breaks every array-typed value
// (uuid.UUID is [16]byte, []byte, json.RawMessage): write "( ?" or "IN ?".
func TestExecModeTypes(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ownerExec(t, d, `CREATE TABLE exec_types (
			id        uuid PRIMARY KEY,
			refs      uuid[] NOT NULL,
			payload   jsonb NOT NULL,
			tags      text[] NOT NULL,
			digest    bytea NOT NULL,
			at        timestamptz NOT NULL,
			maybe_n   integer,
			flag      boolean NOT NULL,
			amount    bigint NOT NULL
		);
		GRANT SELECT, INSERT, UPDATE, DELETE ON exec_types TO app_rw`)

	type row struct {
		ID      uuid.UUID       `gorm:"type:uuid;primaryKey"`
		Refs    db.UUIDArray    `gorm:"type:uuid[]"`
		Payload json.RawMessage `gorm:"type:jsonb"`
		Tags    db.TextArray    `gorm:"type:text[]"`
		Digest  []byte
		At      time.Time
		MaybeN  *int
		Flag    bool
		Amount  int64
	}
	ctx := t.Context()
	table := func() *gorm.DB { return db.Conn(ctx, d.ORM).Table("exec_types") }

	at := time.Date(2026, 10, 3, 9, 30, 0, 123456000, time.UTC)
	seven := 7
	a := row{ID: uuid.NewV7(), Refs: db.UUIDArray{uuid.NewV7(), uuid.NewV7()}, Payload: json.RawMessage(`{"k":"ğüşiöç","n":1}`),
		Tags: db.TextArray{"a", "İz", `comma,"quoted" \ back`}, Digest: []byte{0, 1, 2, 255}, At: at, MaybeN: &seven, Flag: false, Amount: -5}
	b := row{ID: uuid.NewV7(), Refs: db.UUIDArray{}, Payload: json.RawMessage(`[]`), Tags: db.TextArray{}, Digest: []byte{}, At: at.Add(time.Hour)}
	for _, r := range []row{a, b} {
		if err := table().Create(&r).Error; err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	var got row
	if err := table().Where("id = ?", a.ID).Take(&got).Error; err != nil {
		t.Fatalf("take by uuid: %v", err)
	}
	var gotPayload, wantPayload map[string]any
	_ = json.Unmarshal(got.Payload, &gotPayload)
	_ = json.Unmarshal(a.Payload, &wantPayload)
	if got.ID != a.ID || !slices.Equal(got.Refs, a.Refs) || !slices.Equal(got.Tags, a.Tags) || !slices.Equal(got.Digest, a.Digest) ||
		!got.At.Equal(at) || got.MaybeN == nil || *got.MaybeN != 7 || got.Flag || got.Amount != -5 || gotPayload["k"] != wantPayload["k"] {
		t.Fatalf("round trip mismatch:\n got  %+v\n want %+v", got, a)
	}
	var empty row
	if err := table().Where("id = ?", b.ID).Take(&empty).Error; err != nil || len(empty.Refs) != 0 || len(empty.Tags) != 0 || empty.MaybeN != nil {
		t.Fatalf("empty arrays / NULL: %+v, %v", empty, err)
	}

	var ids []uuid.UUID
	if err := table().Where("id IN ?", []uuid.UUID{a.ID, b.ID}).Order("at").Pluck("id", &ids).Error; err != nil {
		t.Fatalf("IN []uuid: %v", err)
	}
	if !slices.Equal(ids, []uuid.UUID{a.ID, b.ID}) {
		t.Fatalf("IN []uuid = %v", ids)
	}
	checks := []struct {
		name  string
		where string
		arg   any
		want  int64
	}{
		{"uuid", "id = ?", a.ID, 1},
		{"= ANY(UUIDArray)", "id = ANY(?)", db.UUIDArray{a.ID, b.ID}, 2},
		{"uuid[] contains", "refs @> ?", db.UUIDArray{a.Refs[0]}, 1},
		{"text[] contains", "tags @> ?", db.TextArray{"İz"}, 1},
		{"text[] overlap", "tags && ?", db.TextArray{`comma,"quoted" \ back`}, 1},
		{"jsonb field", "payload->>'k' = ?", "ğüşiöç", 1},
		{"jsonb contains", "payload @> ?", json.RawMessage(`{"n":1}`), 1},
		{"bytea", "digest = ?", []byte{0, 1, 2, 255}, 1},
		{"timestamptz", "at > ?", at, 1},
		{"nil *int", "maybe_n IS NOT DISTINCT FROM ?", (*int)(nil), 1},
		{"bool", "flag = ?", false, 2},
		{"int64", "amount < ?", int64(0), 1},
		{"named uuid after space", "id IN ( @id )", sql.Named("id", a.ID), 1},
	}
	for _, c := range checks {
		var n int64
		if err := table().Where(c.where, c.arg).Count(&n).Error; err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if n != c.want {
			t.Errorf("%s: count = %d, want %d", c.name, n, c.want)
		}
	}

	// Raw SQL inserts: "( ?" keeps the uuid one value; "(?" expands it.
	id := uuid.NewV7()
	ins := `INSERT INTO exec_types (id, refs, payload, tags, digest, at, flag, amount) VALUES ( ?, ?, ?, ?, ?, ?, true, 1)`
	if err := db.Conn(ctx, d.ORM).Exec(ins, id, db.UUIDArray{id}, json.RawMessage(`{}`), db.TextArray{"x"}, []byte{9}, at).Error; err != nil {
		t.Fatalf("raw insert with '( ?': %v", err)
	}
	trap := `INSERT INTO exec_types (id, refs, payload, tags, digest, at, flag, amount) VALUES (?, '{}', '{}', '{}', '', now(), true, 1)`
	if err := db.Conn(ctx, d.ORM).Exec(trap, uuid.NewV7()).Error; err == nil {
		t.Fatal("GORM no longer expands an array right after '(': revisit the raw SQL rule in package db")
	}

	// Raw pgx on the same pool (outbox relay, advisory locks): []string binds
	// as text[]; []uuid.UUID has no encode plan for an untyped (OID 0)
	// parameter, db.UUIDArray does.
	var rawID uuid.UUID
	var rawTags []string
	err := d.Pool.QueryRow(ctx, `SELECT id, tags FROM exec_types WHERE id = $1 AND refs && $2 AND tags @> $3`,
		a.ID, a.Refs, []string{"a"}).Scan(&rawID, &rawTags)
	if err != nil || rawID != a.ID || !slices.Equal(rawTags, []string(a.Tags)) {
		t.Fatalf("raw pgx: %v %v %v", rawID, rawTags, err)
	}
}

func TestWithTx_NestedJoinsOuter(t *testing.T) {
	t.Parallel()
	d := newKV(t)
	ctx := t.Context()
	pid := func(ctx context.Context) (p int, xid int64) {
		if err := db.Conn(ctx, d.ORM).Raw(`SELECT pg_backend_pid(), txid_current()`).Row().Scan(&p, &xid); err != nil {
			t.Fatal(err)
		}
		return p, xid
	}

	errInner := errors.New("inner failed")
	err := d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := insert(ctx, d, "outer"); err != nil {
			return err
		}
		outerPID, outerXID := pid(ctx)
		return d.Tx.WithTx(ctx, func(ctx context.Context) error {
			innerPID, innerXID := pid(ctx)
			if innerPID != outerPID || innerXID != outerXID {
				t.Errorf("inner tx (pid %d, xid %d) did not join outer (pid %d, xid %d)", innerPID, innerXID, outerPID, outerXID)
			}
			var seen int64
			if err := db.Conn(ctx, d.ORM).Model(&kv{}).Where("val = ?", "outer").Count(&seen).Error; err != nil || seen != 1 {
				t.Errorf("inner tx does not see the outer row: %d, %v", seen, err)
			}
			if err := insert(ctx, d, "inner"); err != nil {
				return err
			}
			return errInner
		})
	})
	if !errors.Is(err, errInner) {
		t.Fatalf("WithTx() = %v, want the inner error", err)
	}
	if n := count(t, d); n != 0 {
		t.Fatalf("inner error must roll back outer and inner rows; %d rows left", n)
	}

	err = d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := insert(ctx, d, "outer"); err != nil {
			return err
		}
		return d.Tx.WithTx(ctx, func(ctx context.Context) error { return insert(ctx, d, "inner") })
	})
	if err != nil || count(t, d) != 2 {
		t.Fatalf("nested commit: err %v, rows %d", err, count(t, d))
	}

	// Serializable inside read committed cannot raise the isolation level.
	err = d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return d.Tx.WithSerializableTx(ctx, func(context.Context) error { return nil })
	})
	if !errors.Is(err, db.ErrNotSerializable) {
		t.Fatalf("WithSerializableTx in WithTx = %v", err)
	}
}

var errBusiness = problem.New(problem.Conflict, "dbtest.business_rule", "Business rule")

func TestWithTx_RollbackOnError(t *testing.T) {
	t.Parallel()
	d := newKV(t)
	err := d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, d, "a"); err != nil {
			return err
		}
		return errBusiness
	})
	if err != errBusiness { //nolint:errorlint // the sentinel must come back unchanged
		t.Fatalf("WithTx() = %v, want the sentinel unchanged", err)
	}
	if n := count(t, d); n != 0 {
		t.Fatalf("%d rows survived the rollback", n)
	}

	// A failed statement aborts the transaction; the error is returned and
	// nothing is committed.
	err = d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, d, "b"); err != nil {
			return err
		}
		return db.Conn(ctx, d.ORM).Exec(`INSERT INTO kv (id, val) VALUES ( ?, NULL)`, uuid.NewV7()).Error
	})
	if db.SQLState(err) != "23502" || count(t, d) != 0 {
		t.Fatalf("statement error: %v, rows %d", err, count(t, d))
	}
}

func TestWithTx_RollbackOnPanic(t *testing.T) {
	t.Parallel()
	d := newKV(t)
	hookRan := false
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("recovered %v, want the original panic", r)
			}
		}()
		_ = d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
			if err := insert(ctx, d, "a"); err != nil {
				return err
			}
			db.AfterCommit(ctx, func(context.Context) { hookRan = true })
			panic("boom")
		})
	}()
	if n := count(t, d); n != 0 {
		t.Fatalf("%d rows survived the panic", n)
	}
	if hookRan {
		t.Fatal("AfterCommit hook ran after a panic")
	}
	// The connection went back to the pool usable.
	if err := d.Tx.WithTx(t.Context(), func(ctx context.Context) error { return insert(ctx, d, "b") }); err != nil || count(t, d) != 1 {
		t.Fatalf("pool unusable after panic: %v", err)
	}
}

func TestAfterCommit_RunsOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	d := newKV(t)
	ctx := t.Context()

	var log []string
	err := d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := insert(ctx, d, "a"); err != nil {
			return err
		}
		db.AfterCommit(ctx, func(hctx context.Context) {
			// After commit: no transaction in the hook context, and the row
			// is visible from another connection.
			if db.InTx(hctx) {
				t.Error("hook context still carries the transaction")
			}
			var n int
			_ = d.Pool.QueryRow(hctx, `SELECT count(*) FROM kv`).Scan(&n)
			log = append(log, "outer-hook")
			if n != 1 {
				t.Errorf("hook ran before the commit was visible (%d rows)", n)
			}
		})
		err := d.Tx.WithTx(ctx, func(ctx context.Context) error {
			db.AfterCommit(ctx, func(context.Context) { log = append(log, "inner-hook") })
			return nil
		})
		log = append(log, "body-done")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"body-done", "outer-hook", "inner-hook"}; !slices.Equal(log, want) {
		t.Fatalf("order = %v, want %v", log, want)
	}

	log = nil
	_ = d.Tx.WithTx(ctx, func(ctx context.Context) error {
		db.AfterCommit(ctx, func(context.Context) { log = append(log, "rolled-back-hook") })
		return errBusiness
	})
	if len(log) != 0 {
		t.Fatalf("hook ran after rollback: %v", log)
	}

	// A cancelled request context does not cancel hooks.
	cctx, cancel := context.WithCancel(ctx)
	err = d.Tx.WithTx(cctx, func(ctx context.Context) error {
		db.AfterCommit(ctx, func(hctx context.Context) {
			if hctx.Err() != nil {
				t.Error("hook context is cancelled")
			}
		})
		return nil
	})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
}

// TestTranslate_RealErrors maps errors produced by PostgreSQL itself,
// including deferred constraints that only fail at COMMIT.
func TestTranslate_RealErrors(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	ownerExec(t, d, `
		CREATE TABLE parent (id uuid PRIMARY KEY);
		CREATE TABLE child (id uuid PRIMARY KEY, parent_id uuid NOT NULL CONSTRAINT child_parent_fkey REFERENCES parent (id));
		CREATE TABLE deferred_uq (k text NOT NULL, CONSTRAINT deferred_uq_k_uq UNIQUE (k) DEFERRABLE INITIALLY DEFERRED);
		CREATE TABLE balanced (txn uuid NOT NULL, amount bigint NOT NULL);
		CREATE FUNCTION balanced_check() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  IF (SELECT sum(amount) FROM balanced WHERE txn = NEW.txn) <> 0 THEN
		    RAISE EXCEPTION 'transaction % is not balanced', NEW.txn
		      USING ERRCODE = 'check_violation', CONSTRAINT = 'balanced_txn_balanced';
		  END IF;
		  RETURN NULL;
		END $$;
		CREATE CONSTRAINT TRIGGER balanced_txn_balanced AFTER INSERT ON balanced
		  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION balanced_check();
		GRANT SELECT, INSERT, DELETE ON parent, child, deferred_uq, balanced TO app_rw`)

	exec := func(sql string, args ...any) error { return db.Translate(db.Conn(ctx, d.ORM).Exec(sql, args...).Error) }
	key := []byte("0123456789abcdef0123456789abcdef")
	if err := exec(`INSERT INTO webhook_inbox (provider, event_key, signature_valid, payload_sha256) VALUES ('iyzico','e1',true,?)`, key); err != nil {
		t.Fatal(err)
	}
	pid := uuid.NewV7()
	if err := exec(`INSERT INTO parent (id) VALUES ( ?)`, pid); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO child (id, parent_id) VALUES ( ?, ?)`, uuid.NewV7(), pid); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		err        error
		want       *problem.Error
		constraint string
	}{
		{"unique", exec(`INSERT INTO webhook_inbox (provider, event_key, signature_valid, payload_sha256) VALUES ('iyzico','e1',false,?)`, key),
			problem.ErrConflict, "webhook_inbox_provider_event_key_uq"},
		{"check", exec(`INSERT INTO outbox (topic, aggregate_type, aggregate_id, payload) VALUES ('Bad Topic','x',?,'{}')`, uuid.NewV7()),
			problem.ErrValidation, "outbox_topic_chk"},
		{"fk missing parent", exec(`INSERT INTO child (id, parent_id) VALUES ( ?, ?)`, uuid.NewV7(), uuid.NewV7()),
			problem.ErrConflict, "child_parent_fkey"},
		{"fk still referenced", exec(`DELETE FROM parent WHERE id = ?`, pid), problem.ErrConflict, "child_parent_fkey"},
		{"not found", db.Translate(db.Conn(ctx, d.ORM).Table("outbox").Where("id = ?", uuid.NewV7()).Take(&map[string]any{}).Error),
			problem.ErrNotFound, ""},
		{"deferred unique at commit", d.Tx.WithTx(ctx, func(ctx context.Context) error {
			for range 2 {
				if err := db.Conn(ctx, d.ORM).Exec(`INSERT INTO deferred_uq (k) VALUES ('x')`).Error; err != nil {
					t.Errorf("deferred insert failed early: %v", err)
				}
			}
			return nil
		}), problem.ErrConflict, "deferred_uq_k_uq"},
		{"constraint trigger at commit", d.Tx.WithTx(ctx, func(ctx context.Context) error {
			return db.Conn(ctx, d.ORM).Exec(`INSERT INTO balanced (txn, amount) VALUES ( ?, 100), ( ?, -40)`, pid, pid).Error
		}), problem.ErrValidation, "balanced_txn_balanced"},
	}
	for _, tt := range tests {
		if !errors.Is(tt.err, tt.want) {
			t.Errorf("%s: error = %v, want %s", tt.name, tt.err, tt.want.Code())
			continue
		}
		if got := db.ConstraintName(tt.err); got != tt.constraint {
			t.Errorf("%s: ConstraintName = %q, want %q", tt.name, got, tt.constraint)
		}
	}
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM deferred_uq`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deferred violation committed %d rows (%v)", n, err)
	}
}

func TestWithSerializableTx_RetriesSerializationFailures(t *testing.T) {
	t.Parallel()
	d := newKV(t)
	attempts := 0
	err := d.Tx.WithSerializableTx(t.Context(), func(ctx context.Context) error {
		attempts++
		var level string
		if err := db.Conn(ctx, d.ORM).Raw(`SHOW transaction_isolation`).Row().Scan(&level); err != nil {
			return err
		}
		if level != "serializable" {
			t.Errorf("isolation = %s", level)
		}
		if err := insert(ctx, d, "try"); err != nil {
			return err
		}
		if attempts < 3 {
			return &pgconn.PgError{Code: db.CodeSerializationFailure, Message: "could not serialize access"}
		}
		return nil
	})
	if err != nil || attempts != 3 || count(t, d) != 1 {
		t.Fatalf("err %v, attempts %d, rows %d; want 3 attempts and only the last commit", err, attempts, count(t, d))
	}

	attempts = 0
	err = d.Tx.WithSerializableTx(t.Context(), func(context.Context) error {
		attempts++
		return &pgconn.PgError{Code: db.CodeDeadlockDetected}
	})
	if !db.IsRetryable(err) || attempts != 4 {
		t.Fatalf("err %v after %d attempts, want a retryable error after 4", err, attempts)
	}
}
