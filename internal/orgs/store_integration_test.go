//go:build integration

package orgs_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// pgWorld inserts users as app_rw.
func pgWorld(d *db.DB) world {
	return world{user: func(t *testing.T) uuid.UUID {
		id := uuid.NewV7()
		email := "u" + strconv.FormatInt(slugSeq.Add(1), 10) + "@example.test"
		if _, err := d.Pool.Exec(t.Context(), `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
			VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, email); err != nil {
			t.Fatal(err)
		}
		return id
	}}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (orgs.Store, world) {
		d := testutil.NewDB(t)
		return orgs.NewGormStore(d.ORM), pgWorld(d)
	})
}

// TestOrgAdminCreate_JoinsCallerTx_Rollback: OrgAdmin.Create writes in the
// caller's transaction (01 §6.3, T-78): when the caller fails afterwards,
// the org and its company profile are gone; when it commits, they stay.
func TestOrgAdminCreate_JoinsCallerTx_Rollback(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	clk := clock.NewFake(t0)
	store := orgs.NewGormStore(d.ORM)
	mod, err := orgs.New(orgs.Deps{
		Store: store, Tx: d.Tx, Outbox: outbox.NewMem(clk), Audit: &audittest.Recorder{}, Clock: clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	boom := errors.New("caller failed after Create")
	var rolled uuid.UUID
	err = d.Tx.WithTx(ctx, func(ctx context.Context) error {
		id, err := mod.Service.Create(ctx, orgs.CreateOrg{Kind: orgs.KindCompany, Name: "Geri Alınan Şirket"})
		if err != nil {
			return err
		}
		rolled = id
		if _, err := store.GetCompany(ctx, id); err != nil { // visible inside the caller's transaction
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("caller transaction = %v", err)
	}
	if _, err := mod.Service.Get(ctx, rolled); !errors.Is(err, orgs.ErrNotFound) {
		t.Fatalf("org after the caller's rollback: %v", err)
	}
	var kept uuid.UUID
	err = d.Tx.WithTx(ctx, func(ctx context.Context) error {
		kept, err = mod.Service.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Kalıcı Kulüp", City: "Ankara"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := mod.Service.Get(ctx, kept)
	if err != nil || s.Kind != orgs.KindClub || s.Slug != "kalici-kulup" {
		t.Fatalf("committed org = %+v, %v", s, err)
	}
	// Two creations with the same name inside one transaction get distinct
	// slugs (ON CONFLICT DO NOTHING keeps the transaction usable).
	err = d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if _, err := mod.Service.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Kalıcı Kulüp"}); err != nil {
			return err
		}
		_, err := mod.Service.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Kalıcı Kulüp"})
		return err
	})
	if err != nil {
		t.Fatalf("same name twice in one transaction: %v", err)
	}
}

func TestStatsStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	statsContract(t, func(t *testing.T) (orgs.Store, world) {
		d := testutil.NewDB(t)
		return orgs.NewGormStore(d.ORM), pgWorld(d)
	})
}
