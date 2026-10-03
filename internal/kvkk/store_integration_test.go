//go:build integration

package kvkk_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/identity"
	"koro/internal/kvkk"
	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var userSeq atomic.Int64

// insertUser writes a users row (data_subject_requests' user and approver
// FKs point to users; identity owns the table).
func insertUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	email := "kvkk" + strconv.FormatInt(userSeq.Add(1), 10) + "@example.test"
	_, err := d.Pool.Exec(t.Context(), `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
		VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, email)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// insertFile writes an attached export files row (export_file_id FK).
func insertFile(t *testing.T, d *db.DB, id uuid.UUID) {
	t.Helper()
	_, err := d.Pool.Exec(t.Context(), `INSERT INTO files (id, purpose, bucket, storage_key, declared_mime, size_bytes, sha256,
		status, scan_status) VALUES ($1, 'dsr_export', 'files', $2, 'application/zip', 10, decode(repeat('00', 32), 'hex'), 'active', 'clean')`,
		id, "dsr_export/"+id.String())
	if err != nil {
		t.Fatalf("insert file: %v", err)
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) storeWorld {
		d := testutil.NewDB(t)
		return storeWorld{s: kvkk.NewGormStore(d.ORM), tx: d.Tx,
			user: func(t *testing.T) uuid.UUID { return insertUser(t, d) },
			file: func(t *testing.T, id uuid.UUID) { insertFile(t, d, id) }}
	})
}

// TestErasure_TombstoneKeepsForeignKeys_PG (02 §0.6, BE-009): app_rw cannot
// delete a users row; identity's erasure turns it into a tombstone with the
// same ID, so the request (and every retained row) keeps a valid foreign
// key, and the account's personal columns are gone.
func TestErasure_TombstoneKeepsForeignKeys_PG(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	store := kvkk.NewGormStore(d.ORM)
	u := insertUser(t, d)
	now := time.Now().UTC().Truncate(time.Microsecond)
	r := kvkk.DSR{ID: uuid.NewV7(), UserID: &u, Type: kvkk.TypeErasure, Channel: kvkk.ChannelInApp, Status: kvkk.StatusReceived,
		ReceivedAt: now, DueAt: now.Add(kvkk.LegalPeriod), IdentityVerifiedAt: &now}
	if err := store.Insert(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u); err == nil || db.SQLState(err) != db.CodeInsufficientPriv {
		t.Fatalf("app_rw deleted a users row (err %v): erasure must anonymise", err)
	}
	erased, err := identity.NewGormStore(d.ORM).EraseUser(ctx, u, now)
	if err != nil || !erased {
		t.Fatalf("EraseUser = %v, %v", erased, err)
	}
	got, err := store.Get(ctx, r.ID)
	if err != nil || got.UserID == nil || *got.UserID != u {
		t.Fatalf("request after erasure = %+v, %v", got, err)
	}
	var status string
	var email *string
	if err := d.Pool.QueryRow(ctx, `SELECT status, email FROM users WHERE id = $1`, u).Scan(&status, &email); err != nil {
		t.Fatal(err)
	}
	if status != "erased" || email != nil {
		t.Fatalf("tombstone = %s, %v", status, email)
	}
	// A new request may still name the tombstone (the FK target exists).
	r2 := r
	r2.ID, r2.Type = uuid.NewV7(), kvkk.TypeInfo
	if err := store.Insert(ctx, r2); err != nil {
		t.Fatalf("FK to the tombstone: %v", err)
	}
	// An unknown user is the module's error, not a 500.
	ghost := uuid.NewV7()
	r3 := r
	r3.ID, r3.UserID = uuid.NewV7(), &ghost
	if err := store.Insert(ctx, r3); !errors.Is(err, kvkk.ErrUnknownUser) {
		t.Fatalf("Insert(unknown user) = %v", err)
	}
}

func TestRecords_Contract_Gorm(t *testing.T) {
	t.Parallel()
	recordsContract(t, func(t *testing.T) recordsWorld {
		d := testutil.NewDB(t)
		return recordsWorld{s: kvkk.NewGormRecords(d.ORM), tx: d.Tx, user: func(t *testing.T) uuid.UUID { return insertUser(t, d) }}
	})
}

// writingSweeper writes in its Sweep (a faulty sweeper on a dry run) and
// calls the consents purge function of 000121 without the approval.
type writingSweeper struct{ orm *gorm.DB }

func (writingSweeper) PolicyCodes() []string {
	return []string{"notification_delivery", "consent_proof"}
}

func (w writingSweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	conn := db.Conn(ctx, w.orm)
	if p.Code == "consent_proof" {
		var n int64
		err := conn.Raw(`SELECT consents_purge( ?, 1000, false)`, now.Add(-p.Retention)).Scan(&n).Error
		return privacy.SweepResult{PolicyCode: p.Code, Count: int(n)}, err
	}
	if err := conn.Exec(`UPDATE retention_policies SET description_tr = 'yazıldı' WHERE code = ?`, p.Code).Error; err != nil {
		return privacy.SweepResult{}, err
	}
	return privacy.SweepResult{PolicyCode: p.Code, Table: "notification_deliveries", Count: 7}, nil
}

// TestRetentionSweep_DryRunRolledBack_PG: on PostgreSQL a dry run is rolled
// back whatever the sweeper writes, a real purge of an append-only table is
// refused by the database until the legal approval (55000, recorded), and
// every policy run lands in destruction_logs.
func TestRetentionSweep_DryRunRolledBack_PG(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	e := newEnv(t, func(dp *kvkk.Deps) { dp.Records, dp.Tx = kvkk.NewGormRecords(d.ORM), d.Tx })
	e.sweepers = []privacy.RetentionSweeper{writingSweeper{orm: d.ORM}}
	if err := e.job(t, kvkk.RetentionSweepJobName).Handle(workerCtx(t)); err != nil {
		t.Fatal(err)
	}
	var desc string
	if err := d.Pool.QueryRow(t.Context(), `SELECT description_tr FROM retention_policies WHERE code = 'notification_delivery'`).Scan(&desc); err != nil {
		t.Fatal(err)
	}
	if desc == "yazıldı" {
		t.Fatal("a dry run's write survived: the transaction was not rolled back")
	}
	rows, err := kvkk.NewGormRecords(d.ORM).DestructionLogs(t.Context(), kvkk.DestructionFilter{}, nil, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("destruction rows = %d, %v", len(rows), err)
	}
	for _, r := range rows {
		switch r.PolicyCode {
		case "notification_delivery":
			if r.RecordCount != 0 || r.Details["dry_run"] != true || r.Details["eligible"] != float64(7) {
				t.Errorf("dry-run row = %+v", r)
			}
		case "consent_proof":
			if r.Details["error"] != "sqlstate_55000" {
				t.Errorf("unapproved purge row = %+v; want the 55000 refusal", r)
			}
		}
	}
}
