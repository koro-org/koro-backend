//go:build integration

package messaging_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/messaging"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
	"koro/internal/students"
)

// pgShares writes the per-company contact sharing in the caller's
// transaction (contact_shared_at of the company's grant, as
// students.ContactSharing does), failing on demand after the write (the
// consent module refusing the text).
type pgShares struct {
	d    *db.DB
	fail *atomic.Bool
}

func (p pgShares) ShareContactWithCompany(ctx context.Context, a authz.Actor, company uuid.UUID, _ students.ConsentProof,
	_ consent.Channel, _ uuid.UUID,
) error {
	res := db.Conn(ctx, p.d.ORM).Exec(`UPDATE profile_access_grants SET contact_shared_at = granted_at
		WHERE student_user_id = @student AND company_org_id = @company AND revoked_at IS NULL`,
		map[string]any{"student": a.UserID, "company": company})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return errors.New("no grant of the company to share with")
	}
	if p.fail.Load() {
		return consent.ErrTextMismatch
	}
	return nil
}

// TestService_Postgres_AcceptShareContactSameTx (T-335/T-345, 01 §6.3;
// BE-089): over PostgreSQL the acceptance, its level 3 grant, the
// company's contact share (contact_shared_at, migration 000136) and
// contact_request.closed commit together; a sharing that fails after its
// write rolls all of them back (the request stays pending, no grant, no
// share, no event).
func TestService_Postgres_AcceptShareContactSameTx(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	companyA := authztest.ID("company_a")
	cmm, stu := actor(t, "company_member"), actor(t, "student")
	pgCompany(t, d, companyA)
	for _, id := range []uuid.UUID{cmm.UserID, stu.UserID} {
		pgUser(t, d, id)
	}
	ent := pgEntitlement(t, d, companyA, 5)
	fail := &atomic.Bool{}
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: outbox.NewRegistry(),
		Queue: queue.NewMem(), Clock: clk, Log: obs.Discard()})
	keeper := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()})
	sf := &studentsFake{discoverable: map[uuid.UUID]bool{stu.UserID: true}}
	res := &resolver{students: sf, known: []uuid.UUID{stu.UserID}}
	of := &orgsFake{orgs: map[uuid.UUID]orgs.OrgSummary{
		companyA: {ID: companyA, Kind: orgs.KindCompany, Name: "Şirket A", Status: orgs.StatusActive},
	}}
	mod, err := messaging.New(messaging.Deps{
		Store: messaging.NewGormStore(d.ORM), Tx: d.Tx, Outbox: ob, Audit: pgAudit{}, Idempotency: keeper, Clock: clk,
		Log: obs.Discard(), Entitlements: pgEntitlements{d: d, ent: ent}, Grants: pgGrants{d: d, fail: &atomic.Bool{}},
		Contacts: pgShares{d: d, fail: fail}, Students: sf, Orgs: of, Pseudonyms: pseudonymsFake{}, Resolve: res.resolve,
	})
	if err != nil {
		t.Fatal(err)
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	v, err := mod.Service.Create(withKey(ctx, cmm, "pg-share-0001"), cmm, companyA, request(companyA, "student"))
	if err != nil {
		t.Fatal(err)
	}

	fail.Store(true)
	_, err = mod.Service.Respond(ctx, stu, v.Request.ID, acceptSharing(contactProof(), consent.ChannelWeb))
	problemtest.WantErr(t, err, consent.ErrTextMismatch)
	if count(`SELECT count(*) FROM contact_requests WHERE id = $1 AND status = 'pending' AND responded_at IS NULL`, v.Request.ID) != 1 ||
		count(`SELECT count(*) FROM profile_access_grants WHERE source_id = $1`, v.Request.ID) != 0 ||
		count(`SELECT count(*) FROM profile_access_grants WHERE contact_shared_at IS NOT NULL`) != 0 ||
		count(`SELECT count(*) FROM outbox WHERE topic = 'contact_request.closed'`) != 0 {
		t.Fatal("a failed sharing did not roll the acceptance back")
	}

	fail.Store(false)
	if _, err := mod.Service.Respond(ctx, stu, v.Request.ID, acceptSharing(contactProof(), consent.ChannelWeb)); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM contact_requests WHERE id = $1 AND status = 'accepted'`, v.Request.ID) != 1 ||
		count(`SELECT count(*) FROM profile_access_grants WHERE source_id = $1 AND student_user_id = $2`, v.Request.ID, stu.UserID) != 1 ||
		count(`SELECT count(*) FROM profile_access_grants WHERE source_id = $1 AND contact_shared_at IS NOT NULL`, v.Request.ID) != 1 ||
		count(`SELECT count(*) FROM outbox WHERE topic = 'contact_request.closed'`) != 1 {
		t.Fatal("the acceptance did not commit with its grant, its sharing and its event")
	}
}
