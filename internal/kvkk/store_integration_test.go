//go:build integration

package kvkk_test

import (
	"errors"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/identity"
	"koro/internal/kvkk"
	"koro/internal/platform/db"
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
