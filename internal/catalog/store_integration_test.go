//go:build integration

package catalog_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var seq atomic.Int64

func nextN() string { return strconv.FormatInt(seq.Add(1), 10) }

// pgWorld inserts the parent rows (organizations, users, payees, events) as app_rw.
func pgWorld(d *db.DB) world {
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	user := func(t *testing.T) uuid.UUID {
		id := uuid.NewV7()
		exec(t, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
			VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, "c"+nextN()+"@example.test")
		return id
	}
	return world{
		org: func(t *testing.T) uuid.UUID {
			id, n := uuid.NewV7(), nextN()
			exec(t, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'club', $2, $3)`, id, "Kulüp "+n, "kulup-"+n)
			return id
		},
		user: user,
		payee: func(t *testing.T, org uuid.UUID) uuid.UUID {
			party, id, by := uuid.NewV7(), uuid.NewV7(), user(t)
			exec(t, `INSERT INTO legal_parties (id, kind, legal_type, title) VALUES ($1, 'payee', 'foundation', $2)`, party, "Vakıf "+nextN())
			exec(t, `INSERT INTO payees (id, legal_party_id, org_id, status, verified_at, verified_by, created_by)
				VALUES ($1, $2, $3, 'active', now(), $4, $4)`, id, party, org, by)
			return id
		},
		event: func(t *testing.T, org uuid.UUID) uuid.UUID {
			id := uuid.NewV7()
			exec(t, `INSERT INTO events (id, org_id, created_by, title, slug, category, format, starts_at, ends_at, status, published_at)
				VALUES ($1, $2, $3, 'Bahar Şenliği', $4, 'social', 'in_person', now() + interval '30 days', now() + interval '31 days',
				'published', now())`, id, org, user(t), "e-"+nextN())
			return id
		},
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (catalog.Store, world) {
		d := testutil.NewDB(t)
		return catalog.NewGormStore(d.ORM), pgWorld(d)
	})
}

// pgService is a catalog store and TxManager over a fresh database, with a
// seller (club org, its payee, a user) and a sellable service.
type pgService struct {
	d       *db.DB
	store   catalog.Store
	w       world
	seller  uuid.UUID
	payee   uuid.UUID
	creator uuid.UUID
	service catalog.ServiceItem
}

func newPG(t *testing.T) pgService {
	t.Helper()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	s := catalog.NewGormStore(d.ORM)
	p := pgService{d: d, store: s, w: w, seller: w.org(t), creator: w.user(t)}
	p.payee = w.payee(t, p.seller)
	sv, err := s.InsertService(t.Context(), catalog.ServiceItem{ID: uuid.NewV7(), Code: "booth_" + nextN(), NameTR: "Stant",
		Category: catalog.CategoryBooth, FulfillmentType: catalog.FulfillOrgBound, DefaultVATRateBP: 2000, VATClass: catalog.VATStandard,
		Active: true, AccountingReviewedAt: &t0})
	if err != nil {
		t.Fatal(err)
	}
	p.service = sv
	return p
}

// activeOffering inserts an approved, active offering (stock total).
func (p pgService) activeOffering(t *testing.T, total *int, perBuyer *int) catalog.Offering {
	t.Helper()
	ctx := t.Context()
	o, err := p.store.InsertOffering(ctx, catalog.Offering{ID: uuid.NewV7(), ServiceID: p.service.ID, SellerKind: catalog.SellerOrg,
		OrgID: p.seller, DefaultPayeeID: &p.payee, Title: "Stant paketi", UnitPriceAmount: 1_000_000, Currency: "TRY", VATRateBP: 2000,
		CompensationType: "cash", QuantityTotal: total, PerBuyerLimit: perBuyer, InvoiceTrigger: catalog.InvoiceOnFulfillment,
		UniversityApprovalStatus: catalog.UniNotRequired, Status: catalog.OfferingDraft, CreatedBy: p.creator})
	if err != nil {
		t.Fatal(err)
	}
	now, approver := time.Now().UTC(), p.w.user(t)
	o.Status, o.SubmittedAt = catalog.OfferingPendingApproval, &now
	if o, err = p.store.UpdateOffering(ctx, o, catalog.OfferingDraft); err != nil {
		t.Fatal(err)
	}
	o.Status, o.ApprovedBy, o.ApprovedAt = catalog.OfferingActive, &approver, &now
	if o, err = p.store.UpdateOffering(ctx, o, catalog.OfferingPendingApproval); err != nil {
		t.Fatal(err)
	}
	return o
}

// module builds the catalog module over the database (real clock: the
// rows' timestamps are the database's).
func (p pgService) module(t *testing.T, held catalog.BuyerHeld) catalog.Module {
	t.Helper()
	clk := clock.Real()
	mod, err := catalog.New(catalog.Deps{
		Store: p.store, Tx: p.d.Tx, Audit: &auditLog{}, Orgs: newFakeOrgs(), Outbox: outbox.NewMem(clk), Queue: queue.NewMem(),
		Clock: clk, Log: slog.New(slog.DiscardHandler), BuyerHeld: held,
		Idempotency: idempotency.New(idempotency.Deps{Tx: p.d.Tx, Store: idempotency.NewGormStore(p.d.ORM), Clock: clk,
			Log: slog.New(slog.DiscardHandler)}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return mod
}

// TestReserve_ConcurrentLastUnit_OneSucceeds_Postgres is the race of
// TestReserve_ConcurrentLastUnit_OneSucceeds on PostgreSQL: 16 concurrent
// transactions for the last 2 units; exactly 2 succeed, the others are
// catalog.sold_out, and the row ends sold_out (row lock + conditional UPDATE).
func TestReserve_ConcurrentLastUnit_OneSucceeds_Postgres(t *testing.T) {
	t.Parallel()
	p := newPG(t)
	o := p.activeOffering(t, ptr(2), nil)
	off := p.module(t, nil).Offerings
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Go(func() { errs[i] = off.Reserve(context.Background(), o.ID, 1, uuid.NewV7()) })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, catalog.ErrSoldOut):
		default:
			t.Errorf("reserve: %v", err)
		}
	}
	got, err := p.store.GetOffering(t.Context(), o.ID, false)
	if err != nil || ok != 2 || got.Status != catalog.OfferingSoldOut || got.QuantitySold != 2 {
		t.Fatalf("reservations = %d, offering %s sold %d (%v); want 2, sold_out, 2", ok, got.Status, got.QuantitySold, err)
	}
}

// TestReserve_PerBuyerLimit_Postgres: the per-buyer check reads BuyerHeld
// under the offering's row lock, in the caller's transaction, so two
// concurrent orders of one buyer cannot both pass the limit: the second
// waits for the first and then counts its unit.
func TestReserve_PerBuyerLimit_Postgres(t *testing.T) {
	t.Parallel()
	p := newPG(t)
	o := p.activeOffering(t, ptr(10), ptr(1))
	buyer := uuid.NewV7()
	var mu sync.Mutex
	held := 0 // the orders "table" of the test: written in the reserving transaction
	off := p.module(t, func(context.Context, uuid.UUID, uuid.UUID) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return held, nil
	}).Offerings
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Go(func() {
			errs[i] = p.d.Tx.WithTx(context.Background(), func(ctx context.Context) error {
				if err := off.Reserve(ctx, o.ID, 1, buyer); err != nil {
					return err
				}
				// The order line of the buyer, committed with the reservation.
				db.AfterCommit(ctx, func(context.Context) { mu.Lock(); held++; mu.Unlock() })
				time.Sleep(20 * time.Millisecond) // hold the row lock: the others wait
				return nil
			})
		})
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, catalog.ErrPerBuyerLimit):
		default:
			t.Errorf("reserve: %v", err)
		}
	}
	got, _ := p.store.GetOffering(t.Context(), o.ID, false)
	if ok != 1 || got.QuantitySold != 1 {
		t.Fatalf("reservations of one buyer with limit 1 = %d, sold %d", ok, got.QuantitySold)
	}
}

// TestStore_DatabaseRules_Postgres: the database's own answers of the
// catalog rules map to the module errors — the creator's approval
// (offerings_approver_chk → 403 authz.four_eyes_violation), an
// overlapping period (→ 409 catalog.policy_overlap), a payee or event of
// another org (composite FKs → 422 catalog.parent_mismatch, 06 H-38/H-55),
// and a closed policy takes no change (koro_policy_close_only).
func TestStore_DatabaseRules_Postgres(t *testing.T) {
	t.Parallel()
	p := newPG(t)
	ctx := t.Context()
	other := p.w.org(t)
	foreignEvent, foreignPayee := p.w.event(t, other), p.w.payee(t, other)
	base := catalog.Offering{ID: uuid.NewV7(), ServiceID: p.service.ID, SellerKind: catalog.SellerOrg, OrgID: p.seller,
		DefaultPayeeID: &p.payee, Title: "Stant paketi", UnitPriceAmount: 1_000, Currency: "TRY", VATRateBP: 2000, CompensationType: "cash",
		InvoiceTrigger: catalog.InvoiceOnFulfillment, UniversityApprovalStatus: catalog.UniNotRequired, Status: catalog.OfferingDraft,
		CreatedBy: p.creator}
	withEvent := base
	withEvent.ID, withEvent.EventID = uuid.NewV7(), &foreignEvent
	if _, err := p.store.InsertOffering(ctx, withEvent); !errors.Is(err, catalog.ErrParentMismatch) {
		t.Fatalf("another org's event: %v, want catalog.parent_mismatch", err)
	}
	withPayee := base
	withPayee.ID, withPayee.DefaultPayeeID = uuid.NewV7(), &foreignPayee
	if _, err := p.store.InsertOffering(ctx, withPayee); !errors.Is(err, catalog.ErrParentMismatch) {
		t.Fatalf("another org's payee: %v, want catalog.parent_mismatch", err)
	}
	o, err := p.store.InsertOffering(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	o.Status, o.SubmittedAt = catalog.OfferingPendingApproval, &now
	if o, err = p.store.UpdateOffering(ctx, o, catalog.OfferingDraft); err != nil {
		t.Fatal(err)
	}
	forged := o
	forged.Status, forged.ApprovedBy, forged.ApprovedAt = catalog.OfferingActive, &p.creator, &now
	if _, err := p.store.UpdateOffering(ctx, forged, catalog.OfferingPendingApproval); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("approval by the creator: %v, want authz.four_eyes_violation", err)
	}

	pol := catalog.CommercePolicy{ID: uuid.NewV7(), Scope: catalog.ScopeSellerOrg, ScopeID: &p.seller, ModelCode: catalog.ModelA,
		CollectionMethod: catalog.CollectBankTransfer, CommissionRateBP: 1000, CommissionBasis: catalog.BasisNetExclVAT,
		CommissionPayer: catalog.PayerSeller, CommissionVATRateBP: 2000, InvoiceAutomation: catalog.InvoiceManual, DisputeWindowDays: 7,
		ValidFrom: now, CreatedBy: p.creator}
	first, err := p.store.InsertPolicy(ctx, pol)
	if err != nil {
		t.Fatal(err)
	}
	overlap := pol
	overlap.ID, overlap.ValidFrom = uuid.NewV7(), now.Add(time.Hour)
	if _, err := p.store.InsertPolicy(ctx, overlap); !errors.Is(err, catalog.ErrPolicyOverlap) {
		t.Fatalf("overlapping period: %v, want catalog.policy_overlap", err)
	}
	if _, err := p.store.ClosePolicy(ctx, first.ID, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.d.Pool.Exec(ctx, `UPDATE commerce_policies SET commission_rate_bp = 500 WHERE id = $1`, first.ID); db.SQLState(err) != db.CodeRestrictViolation {
		t.Fatalf("changing a policy in place: SQLSTATE %q, want %s", db.SQLState(err), db.CodeRestrictViolation)
	}
	if _, err := p.store.ClosePolicy(ctx, first.ID, now.Add(48*time.Hour)); !errors.Is(err, catalog.ErrInvalidState) {
		t.Fatalf("closing twice: %v", err)
	}
}
