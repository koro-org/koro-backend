//go:build integration

package orders_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/entitlements"
	"koro/internal/files"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var seq atomic.Int64

func nextN() string { return strconv.FormatInt(seq.Add(1), 10) }

// pgIBAN is a valid TR IBAN no other call returned (bank code 99999).
func pgIBAN() string {
	bban := "99999" + "0" + fmt.Sprintf("%016d", seq.Add(1))
	rem := 0
	for _, part := range []string{bban, "2927", "00"} {
		for i := range len(part) {
			rem = (rem*10 + int(part[i]-'0')) % 97
		}
	}
	return fmt.Sprintf("TR%02d%s", 98-rem, bban)
}

// policyFrom is the start of the PostgreSQL policies (before t0).
var policyFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// pgWorld inserts the parent rows as app_rw.
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
			VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, "o"+nextN()+"@example.test")
		return id
	}
	orgOf := func(t *testing.T, kind string) uuid.UUID {
		id, n := uuid.NewV7(), nextN()
		exec(t, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, $2, $3, $4)`, id, kind, "Org "+n, "org-o-"+n)
		return id
	}
	return world{
		buyer: func(t *testing.T) buyerRef {
			b := buyerRef{org: orgOf(t, "company"), user: user(t), party: uuid.NewV7()}
			exec(t, `INSERT INTO legal_parties (id, kind, legal_type, org_id, title) VALUES ($1, 'buyer_company', 'company', $2, 'Örnek A.Ş.')`,
				b.party, b.org)
			return b
		},
		seller: func(t *testing.T) sellerRef {
			s := sellerRef{org: orgOf(t, "club"), payee: uuid.NewV7()}
			party := uuid.NewV7()
			exec(t, `INSERT INTO legal_parties (id, kind, legal_type, title) VALUES ($1, 'payee', 'foundation', $2)`, party, "Vakıf "+nextN())
			exec(t, `INSERT INTO payees (id, legal_party_id, org_id, status, verified_at, verified_by, created_by)
				VALUES ($1, $2, $3, 'active', now(), $4, $5)`, s.payee, party, s.org, user(t), user(t))
			return s
		},
		offering: func(t *testing.T, s sellerRef) offeringRef {
			o := offeringRef{id: uuid.NewV7(), service: uuid.NewV7(), policy: uuid.NewV7()}
			exec(t, `INSERT INTO services (id, code, name_tr, category, fulfillment_type, accounting_reviewed_at)
				VALUES ($1, $2, 'Stant', 'booth', 'org_bound', now())`, o.service, "booth_o"+nextN())
			exec(t, `INSERT INTO offerings (id, service_id, seller_kind, org_id, default_payee_id, title, unit_price_amount, vat_rate_bp,
				status, submitted_at, approved_by, approved_at, created_by)
				VALUES ($1, $2, 'org', $3, $4, 'Bahar şenliği stant', 1000000, 2000, 'active', now(), $5, now(), $6)`,
				o.id, o.service, s.org, s.payee, user(t), user(t))
			exec(t, `INSERT INTO commerce_policies (id, scope, scope_id, model_code, collection_method, commission_rate_bp, commission_basis,
				commission_payer, valid_from, created_by)
				VALUES ($1, 'offering', $2, 'A', 'bank_transfer', 1000, 'net_excl_vat', 'seller', $3, $4)`, o.policy, o.id, policyFrom, user(t))
			return o
		},
		event: func(t *testing.T, org uuid.UUID) uuid.UUID {
			id := uuid.NewV7()
			exec(t, `INSERT INTO events (id, org_id, created_by, title, slug, category, format, starts_at, ends_at, status, published_at)
				VALUES ($1, $2, $3, 'Bahar Şenliği', $4, 'social', 'in_person', now() + interval '30 days', now() + interval '31 days',
				'published', now())`, id, org, user(t), "e-o-"+nextN())
			return id
		},
		payout: func(t *testing.T, s sellerRef, lines map[uuid.UUID]int64) uuid.UUID {
			file, acct, id, iban := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), pgIBAN()
			exec(t, `INSERT INTO files (id, purpose, bucket, storage_key, declared_mime) VALUES ($1, 'bank_account_evidence', 'koro-files-dev', $2, 'application/pdf')`,
				file, "bank_account_evidence/2026/10/"+file.String())
			exec(t, `INSERT INTO payee_bank_accounts (id, payee_id, org_id, iban, holder_name, evidence_file_id, status, submitted_by, submitted_by_admin)
				VALUES ($1, $2, $3, $4, 'Ankara Üniversitesi Vakfı', $5, 'pending_approval', $6, true)`, acct, s.payee, s.org, iban, file, user(t))
			exec(t, `UPDATE payee_bank_accounts SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1`, acct, user(t))
			var total int64
			for _, a := range lines {
				total += a
			}
			tx, err := d.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(t.Context(), `INSERT INTO payouts (id, payee_id, org_id, bank_account_id, iban_snapshot, holder_snapshot, amount, created_by)
				VALUES ($1, $2, $3, $4, $5, 'Ankara Üniversitesi Vakfı', $6, $7)`, id, s.payee, s.org, acct, iban, total, user(t)); err != nil {
				t.Fatal(err)
			}
			for line, a := range lines {
				if _, err := tx.Exec(t.Context(), `INSERT INTO payout_items (payout_id, order_line_id, payee_id, amount) VALUES ($1, $2, $3, $4)`,
					id, line, s.payee, a); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			return id
		},
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (orders.Store, world) {
		d := testutil.NewDB(t)
		return orders.NewGormStore(d.ORM), pgWorld(d)
	})
}

// pgEnv is the orders module over PostgreSQL with the real catalog and
// entitlements stores; orgs, parties and payees are fakes over the rows
// the world inserted.
type pgEnv struct {
	d      *db.DB
	clk    *clock.Fake
	w      world
	buyer  buyerRef
	seller sellerRef
	cat    catalog.Store
	ents   entitlements.Store
	outbox *outbox.Mem
	mod    orders.Module
	keeper *idempotency.Keeper
	files  files.Service // the files module over PostgreSQL (the delivery evidence, BE-093)
}

// newPGEnv builds the env; opts edit the orders dependencies before New.
func newPGEnv(t *testing.T, opts ...func(*orders.Deps)) *pgEnv {
	t.Helper()
	d := testutil.NewDB(t)
	clk := clock.NewFake(t0)
	p := &pgEnv{d: d, clk: clk, w: pgWorld(d), cat: catalog.NewGormStore(d.ORM), ents: entitlements.NewGormStore(d.ORM), outbox: outbox.NewMem(clk)}
	p.buyer, p.seller = p.w.buyer(t), p.w.seller(t)
	fo := newFakeOrgs()
	fo.m[p.buyer.org] = orgs.OrgSummary{ID: p.buyer.org, Kind: orgs.KindCompany, Name: "Şirket", Status: orgs.StatusActive}
	fo.m[p.seller.org] = orgs.OrgSummary{ID: p.seller.org, Kind: orgs.KindClub, Name: "Kulüp", Status: orgs.StatusActive}
	fp := &fakeParties{m: map[uuid.UUID]parties.LegalParty{}}
	party := buyerParty("company_a", parties.StatusVerified)
	party.ID, party.OrgID = p.buyer.party, &p.buyer.org
	fp.add(party)
	payees := &fakePayees{m: map[uuid.UUID]parties.Payee{p.seller.payee: {ID: p.seller.payee, OrgID: p.seller.org, Status: parties.PayeeActive, IsDefault: true}}}
	p.keeper = idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: discardLog()})
	var mod orders.Module
	cat, err := catalog.New(catalog.Deps{Store: p.cat, Tx: d.Tx, Audit: &auditLog{}, Idempotency: p.keeper, Orgs: fo, Outbox: p.outbox,
		Queue: queue.NewMem(), Clock: clk, Log: discardLog(),
		BuyerHeld: func(ctx context.Context, off, buyer uuid.UUID) (int, error) { return mod.BuyerHeld(ctx, off, buyer) }})
	if err != nil {
		t.Fatal(err)
	}
	ents, err := entitlements.New(entitlements.Deps{Store: p.ents, Tx: d.Tx, Audit: &auditLog{}, Idempotency: p.keeper, Clock: clk, Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	fm, err := files.New(files.Deps{Store: files.NewGormStore(d.ORM), Objects: storage.NewMem(clk), Tx: d.Tx, Outbox: p.outbox,
		Queue: queue.NewMem(), Idempotency: p.keeper, Clock: clk, Log: discardLog(), Buckets: evidenceBuckets, Audit: &auditLog{}})
	if err != nil {
		t.Fatal(err)
	}
	p.files = fm.Service
	od := orders.Deps{Store: orders.NewGormStore(d.ORM), Tx: d.Tx, Audit: &auditLog{}, Idempotency: p.keeper, Orgs: fo,
		Offerings: cat.Offerings, Parties: fp, Payees: payees, Entitlements: ents.Service, Outbox: p.outbox, Clock: clk, Log: discardLog(),
		Files: p.files}
	for _, o := range opts {
		o(&od)
	}
	mod, err = orders.New(od)
	if err != nil {
		t.Fatal(err)
	}
	p.mod = mod
	return p
}

// buyerActor is a company admin of the PostgreSQL buyer (the user row the
// order references).
func (p *pgEnv) buyerActor(t *testing.T) context.Context {
	t.Helper()
	a := actor(t, "company_admin")
	a.UserID = p.buyer.user
	a.Memberships = []authz.Membership{{OrgID: p.buyer.org, OrgKind: authz.OrgCompany, Role: authz.MemberCompanyAdmin}}
	return ctxOf(t, a)
}

func (p *pgEnv) create(ctx context.Context, lines ...orders.LineInput) ([]orders.OrderView, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return p.mod.Service.CreateOrder(ctx, a, p.buyer.org, orders.CreateInput{BuyerPartyID: p.buyer.party, Lines: lines})
}

// pgExec runs a statement as app_rw.
func pgExec(t *testing.T, d *db.DB, sql string, args ...any) {
	t.Helper()
	if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// pgUser inserts an active user and returns its ID.
func pgUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	pgExec(t, d, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
		VALUES ($1, 'active', $2, $2, 'Can', 'Demir', 'club')`, id, "s"+nextN()+"@example.test")
	return id
}

// eventOffering is an active offering of the seller bound to an event of
// the seller org (06 H-55: the line carries the event under the composite
// FK with its seller org), priced like world.offering.
func (p *pgEnv) eventOffering(t *testing.T) (offeringRef, uuid.UUID) {
	t.Helper()
	off := p.w.offering(t, p.seller)
	ev := p.w.event(t, p.seller.org)
	pgExec(t, p.d, `UPDATE offerings SET event_id = $2 WHERE id = $1`, off.id, ev)
	return off, ev
}

// platformOffering is an active platform offering of the platform-native
// targeted post quota (5 posts per unit, 90 days), without a payee, under
// a model A policy of its own.
func (p *pgEnv) platformOffering(t *testing.T) uuid.UUID {
	t.Helper()
	n := nextN()
	svc, off, by, approver := uuid.NewV7(), uuid.NewV7(), pgUser(t, p.d), pgUser(t, p.d)
	var plat uuid.UUID // the seeded platform org (000031; one per database)
	if err := p.d.Pool.QueryRow(t.Context(), `SELECT id FROM organizations WHERE kind = 'platform'`).Scan(&plat); err != nil {
		t.Fatal(err)
	}
	pgExec(t, p.d, `INSERT INTO services (id, code, name_tr, category, fulfillment_type, entitlement_template, accounting_reviewed_at)
		VALUES ($1, $2, 'Hedefli gönderi', 'targeted_post_quota', 'platform_native',
		'{"feature":"targeted_post","kind":"quota","quantity_per_unit":5,"valid_days":90}', now())`, svc, "tp_o"+n)
	pgExec(t, p.d, `INSERT INTO offerings (id, service_id, seller_kind, org_id, title, unit_price_amount, vat_rate_bp,
		status, submitted_at, approved_by, approved_at, created_by)
		VALUES ($1, $2, 'platform', $3, '5 hedefli gönderi', 250000, 2000, 'active', now(), $4, now(), $5)`, off, svc, plat, approver, by)
	pgExec(t, p.d, `INSERT INTO commerce_policies (id, scope, scope_id, model_code, collection_method, commission_rate_bp, commission_basis,
		commission_payer, valid_from, created_by)
		VALUES ($1, 'offering', $2, 'A', 'bank_transfer', 0, 'net_excl_vat', 'seller', $3, $4)`, uuid.NewV7(), off, policyFrom, by)
	return off
}

// sellerActor is a club admin of the PostgreSQL seller (a user row, since
// fulfillment_marked_by references users).
func (p *pgEnv) sellerActor(t *testing.T) (authz.Actor, context.Context) {
	t.Helper()
	a := actor(t, "club_admin")
	a.UserID = pgUser(t, p.d)
	a.Memberships = []authz.Membership{{OrgID: p.seller.org, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}
	return a, ctxOf(t, a)
}

// TestCreateOrder_Postgres_Chain: a placement commits on PostgreSQL (the
// deferred orders_totals_match_lines check, the composite FKs of the line —
// its event bound with its seller org, 06 H-55 — the frozen copy), the
// platform-native right goes pending → active with the payment, and the
// delivery runs its targeted updates (mark, confirm) under the CHECKs.
func TestCreateOrder_Postgres_Chain(t *testing.T) {
	t.Parallel()
	p := newPGEnv(t)
	off, ev := p.eventOffering(t)
	quota := p.platformOffering(t)
	ctx := p.buyerActor(t)
	vs, err := p.create(ctx, line(off.id, 2), line(quota, 1))
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if len(vs) != 1 || len(vs[0].Lines) != 2 {
		t.Fatalf("orders = %+v", vs)
	}
	o := vs[0].Order
	var booth, right orders.Line
	for _, l := range vs[0].Lines {
		if l.OfferingID == off.id {
			booth = l
		} else {
			right = l
		}
	}
	if o.PayableAmount != 2_700_000 || booth.CommercePolicyID != off.policy || booth.SellerNetAmount != 2_160_000 ||
		booth.EventID == nil || *booth.EventID != ev || right.PayeeID != nil || right.FulfillmentStatus != orders.FulfillNotApplicable {
		t.Fatalf("order = %+v", vs[0])
	}
	got, err := p.cat.GetOffering(t.Context(), off.id, false)
	if err != nil || got.QuantitySold != 2 {
		t.Fatalf("units sold = %d, %v", got.QuantitySold, err)
	}
	rights, err := p.ents.BySource(t.Context(), entitlements.SourceOrderLine, right.ID)
	if err != nil || len(rights) != 1 || rights[0].Status != entitlements.StatusPending {
		t.Fatalf("pending right = %+v, %v", rights, err)
	}
	eff, err := p.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 400_000)
	if err != nil || eff.Paid {
		t.Fatalf("partial = %+v, %v", eff, err)
	}
	if eff, err = p.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 2_300_000); err != nil || !eff.Paid || len(eff.Lines) != 2 {
		t.Fatalf("full = %+v, %v", eff, err)
	}
	if found, ok, err := p.mod.Orders.FindByReference(t.Context(), " "+o.OrderNo[:5]+"-"+o.OrderNo[5:]); err != nil || !ok ||
		found.Status != orders.StatusInFulfilment {
		t.Fatalf("FindByReference = %+v, %v, %v", found, ok, err)
	}
	if rights, err = p.ents.BySource(t.Context(), entitlements.SourceOrderLine, right.ID); err != nil || len(rights) != 1 ||
		rights[0].Status != entitlements.StatusActive {
		t.Fatalf("right after payment = %+v, %v", rights, err)
	}
	sa, sctx := p.sellerActor(t)
	if _, err := p.mod.Service.MarkLineFulfilled(sctx, sa, booth.ID, orders.MarkInput{Note: "Stant kuruldu"}); err != nil {
		t.Fatalf("MarkLineFulfilled: %v", err)
	}
	ba, err := authz.ActorFrom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lv, err := p.mod.Service.ConfirmLine(ctx, ba, booth.ID)
	if err != nil || lv.Line.FulfillmentStatus != orders.FulfillFulfilled || lv.OrderStatus != orders.StatusCompleted {
		t.Fatalf("ConfirmLine = %+v, %v", lv, err)
	}
	if events := p.outbox.Events(); len(events) == 0 || events[0].Topic != domainevent.TopicOrderPlaced {
		t.Fatalf("outbox = %+v", p.outbox.Topics())
	}
}

// TestModelC_Postgres: a model C order (fee to Koro, the gross to the
// payee) is paid by the direct confirmation on PostgreSQL
// (orders_paid_full_chk: paid_amount = koro_collect_amount = the fee).
func TestModelC_Postgres(t *testing.T) {
	t.Parallel()
	p := newPGEnv(t)
	off := p.w.offering(t, p.seller)
	pgExec(t, p.d, `UPDATE commerce_policies SET valid_to = $2 WHERE id = $1`, off.policy, policyFrom.Add(time.Hour))
	pgExec(t, p.d, `INSERT INTO commerce_policies (id, scope, scope_id, model_code, collection_method, commission_rate_bp, commission_basis,
		commission_payer, valid_from, created_by)
		VALUES ($1, 'offering', $2, 'C', 'direct_to_payee', 1000, 'net_excl_vat', 'buyer', $3, $4)`,
		uuid.NewV7(), off.id, policyFrom.Add(time.Hour), p.buyer.user)
	vs, err := p.create(p.buyerActor(t), line(off.id, 1))
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	o := vs[0].Order
	if o.ModelCode != orders.ModelC || o.KoroCollectAmount != 120_000 || o.DirectToPayeeAmount != 1_200_000 {
		t.Fatalf("model C order = %+v", o)
	}
	if eff, err := p.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 120_000); err != nil || eff.Paid {
		t.Fatalf("fee = %+v, %v", eff, err)
	}
	eff, err := p.mod.Orders.ConfirmDirectPayment(t.Context(), o.ID, uuid.NewV7())
	if err != nil || !eff.Paid || len(eff.Lines) != 1 {
		t.Fatalf("direct confirmation = %+v, %v", eff, err)
	}
	if got, err := p.mod.Orders.ForPayment(t.Context(), o.ID); err != nil || got.Status != orders.StatusPaid || got.PaidAmount != 120_000 {
		t.Fatalf("order = %+v, %v", got, err)
	}
}

// TestCreateOrder_ConcurrentPerBuyerLimit_Postgres (BE-038's late binding):
// six concurrent orders of one buyer for an offering limited to one unit
// per buyer: exactly one is placed, the others are 409
// catalog.per_buyer_limit (BuyerHeld reads the committed lines under the
// offering's row lock).
func TestCreateOrder_ConcurrentPerBuyerLimit_Postgres(t *testing.T) {
	t.Parallel()
	p := newPGEnv(t)
	off := p.w.offering(t, p.seller)
	if _, err := p.d.Pool.Exec(t.Context(), `UPDATE offerings SET per_buyer_limit = 1, quantity_total = 10 WHERE id = $1`, off.id); err != nil {
		t.Fatal(err)
	}
	ctx := p.buyerActor(t)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Go(func() { _, errs[i] = p.create(ctx, line(off.id, 1)) })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, catalog.ErrPerBuyerLimit):
		default:
			t.Errorf("create: %v", err)
		}
	}
	got, err := p.cat.GetOffering(t.Context(), off.id, false)
	if err != nil || ok != 1 || got.QuantitySold != 1 {
		t.Fatalf("placed %d, units sold %d (%v); want 1 and 1", ok, got.QuantitySold, err)
	}
}

// TestCreateOrder_SameIdempotencyKey_Concurrent_Postgres (06 H-5): five
// concurrent retries with one key place one order and reserve one unit;
// the others wait on the key and replay it.
func TestCreateOrder_SameIdempotencyKey_Concurrent_Postgres(t *testing.T) {
	t.Parallel()
	p := newPGEnv(t)
	off := p.w.offering(t, p.seller)
	ctx := idempotency.WithKey(p.buyerActor(t), idempotency.Key{Scope: p.buyer.user.String() + ":POST /orgs/{org_id}/orders",
		Value: "pg-order-key", Status: 201})
	var wg sync.WaitGroup
	got := make([][]orders.OrderView, 5)
	errs := make([]error, 5)
	for i := range got {
		wg.Go(func() { got[i], errs[i] = p.create(ctx, line(off.id, 1)) })
	}
	wg.Wait()
	var first uuid.UUID
	for i, err := range errs {
		if err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
		if first == uuid.Nil() {
			first = got[i][0].Order.ID
		}
		if got[i][0].Order.ID != first {
			t.Fatalf("retry %d placed another order", i)
		}
	}
	o, err := p.cat.GetOffering(t.Context(), off.id, false)
	if err != nil || o.QuantitySold != 1 {
		t.Fatalf("units sold = %d, %v", o.QuantitySold, err)
	}
}

// TestExpireUnpaid_Postgres: the expiry job ends an unpaid order on
// PostgreSQL (FOR UPDATE SKIP LOCKED), releases its units and leaves the
// state the CHECKs accept.
func TestExpireUnpaid_Postgres(t *testing.T) {
	t.Parallel()
	p := newPGEnv(t)
	off := p.w.offering(t, p.seller)
	vs, err := p.create(p.buyerActor(t), line(off.id, 3))
	if err != nil {
		t.Fatal(err)
	}
	p.clk.Advance(orders.DefaultPaymentWindow + time.Minute)
	for _, j := range p.mod.Jobs() {
		if j.Name == orders.ExpireJobName {
			if err := j.Handle(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	o, err := p.mod.Orders.ForPayment(t.Context(), vs[0].Order.ID)
	if err != nil || o.Status != orders.StatusExpired {
		t.Fatalf("order = %+v, %v", o, err)
	}
	got, err := p.cat.GetOffering(t.Context(), off.id, false)
	if err != nil || got.QuantitySold != 0 {
		t.Fatalf("units sold after expiry = %d, %v", got.QuantitySold, err)
	}
}
