package payments_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/catalog"
	"koro/internal/entitlements"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/ledger"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/payments"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

// Test IBANs (valid TR IBANs, bank code 00061 / 00099).
const (
	ibanKoro   = "TR330006100519786457841326"
	ibanKoro2  = "TR940001000999990123456789"
	ibanKoro3  = "TR190001200000000001234567"
	ibanSender = "TR100006200000000006295938"
	ibanOther  = "TR680006400000000009876543"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func id(name string) uuid.UUID { return authztest.ID(name) }

func actor(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func ctxOf(t *testing.T, a authz.Actor) context.Context { return authz.WithActor(t.Context(), a) }

// good is the proof fakeReauth accepts.
var good = identity.ReauthProof{Password: "matrix-reauth-password"}

// ---- fakes

// fakeReauth accepts good, refuses a missing or wrong proof.
type fakeReauth struct{}

func (fakeReauth) Verify(_ context.Context, _ uuid.UUID, p identity.ReauthProof) (string, error) {
	switch {
	case p == identity.ReauthProof{}:
		return "", identity.ErrReauthRequired
	case p != good:
		return "", identity.ErrReauthFailed
	}
	return "password", nil
}

// fakeParties knows Koro's own party.
type fakeParties struct{ platform uuid.UUID }

func (f fakeParties) Platform(context.Context) (parties.LegalParty, error) {
	if f.platform == uuid.Nil() {
		return parties.LegalParty{}, parties.ErrNotFound
	}
	return parties.LegalParty{ID: f.platform, Kind: parties.KindPlatform, Title: "Koro Teknoloji A.Ş."}, nil
}

// fakeFiles holds uploads and their owners.
type fakeFiles struct {
	mu    sync.Mutex
	m     map[uuid.UUID]files.FileMeta
	added func(fid uuid.UUID, purpose files.Purpose) // PostgreSQL: the files row
}

func (f *fakeFiles) add(purpose files.Purpose) uuid.UUID {
	fid := uuid.NewV7()
	if f.added != nil {
		f.added(fid, purpose)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[fid] = files.FileMeta{ID: fid, Purpose: purpose, Status: files.StatusActive, ScanStatus: files.ScanClean, AttachedEntityID: uuid.Nil()}
	return fid
}

func (f *fakeFiles) Get(_ context.Context, fid uuid.UUID) (files.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.m[fid]
	if !ok {
		return files.FileMeta{}, files.ErrNotFound
	}
	return m, nil
}

// PresignGet answers a fake URL of a clean file (files.ErrNotClean for one
// not scanned clean).
func (f *fakeFiles) PresignGet(_ context.Context, fid uuid.UUID, ttl time.Duration) (files.Download, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.m[fid]
	switch {
	case !ok:
		return files.Download{}, files.ErrNotFound
	case m.ScanStatus != files.ScanClean:
		return files.Download{}, files.ErrNotClean
	}
	return files.Download{URL: "https://files.example.test/" + fid.String(), ExpiresAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC).Add(ttl)}, nil
}

func (f *fakeFiles) Attach(_ context.Context, _ authz.Actor, fid uuid.UUID, o files.Owner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.m[fid]
	switch {
	case !ok:
		return files.ErrNotFound
	case m.Purpose != o.Purpose:
		return files.ErrPurposeMismatch
	case m.AttachedModule != "" && (m.AttachedModule != o.Module || m.AttachedEntityID != o.EntityID):
		return files.ErrAlreadyAttached
	}
	m.AttachedModule, m.AttachedEntityID = o.Module, o.EntityID
	f.m[fid] = m
	return nil
}

func (f *fakeFiles) owner(fid uuid.UUID) (string, uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[fid].AttachedModule, f.m[fid].AttachedEntityID
}

// fakeOrgs answers from the matrix personas (authztest).
type fakeOrgs struct{}

func (fakeOrgs) ActiveMemberIDs(_ context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.Actor.MemberOf(orgID) {
			out = append(out, p.User())
		}
	}
	return out, nil
}

func (fakeOrgs) HasRole(_ context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error) {
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.User() == userID && p.Actor.HasOrgRole(orgID, roles...) {
			return true, nil
		}
	}
	return false, nil
}

// fakeStaff: two finance admins, two platform admins (the personas).
type fakeStaff struct{}

func (fakeStaff) FinanceAdmins(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{id("finance_admin"), id("finance_admin_2")}, nil
}

func (fakeStaff) PlatformAdmins(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{id("platform_admin"), id("platform_admin_2"), id("finance_admin")}, nil
}

// fakeUsers resolves every user to an address of its own.
type fakeUsers struct{}

func (fakeUsers) Contact(_ context.Context, u uuid.UUID) (identity.UserContact, error) {
	return identity.UserContact{ID: u, Email: u.String() + "@koro.test", FirstName: "Ada", Locale: "tr"}, nil
}

type fakeSender struct {
	mu   sync.Mutex
	sent []notify.Message
	keys map[string]bool
}

func (f *fakeSender) Send(_ context.Context, m notify.Message) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys == nil {
		f.keys = map[string]bool{}
	}
	if !f.keys[m.DedupeKey] {
		f.keys[m.DedupeKey] = true
		f.sent = append(f.sent, m)
	}
	return uuid.NewV7(), nil
}

func (f *fakeSender) to(tmpl string, ch notify.Channel) []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for _, m := range f.sent {
		if m.Template == tmpl && m.Channel == ch {
			out = append(out, *m.UserID)
		}
	}
	return out
}

// ---- orders stubs (the capture path never reaches them)

type noOrgs struct{}

func (noOrgs) Get(context.Context, uuid.UUID) (orgs.OrgSummary, error) {
	return orgs.OrgSummary{}, orgs.ErrNotFound
}
func (noOrgs) GetMany(context.Context, []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error) {
	return map[uuid.UUID]orgs.OrgSummary{}, nil
}
func (noOrgs) ActiveMemberIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
func (noOrgs) HasRole(context.Context, uuid.UUID, uuid.UUID, ...authz.MemberRole) (bool, error) {
	return false, nil
}

type noOfferings struct{}

func (noOfferings) ForCheckout(context.Context, uuid.UUID) (catalog.CheckoutOffering, error) {
	return catalog.CheckoutOffering{}, catalog.ErrNotFound
}
func (noOfferings) ResolvePolicy(context.Context, uuid.UUID, time.Time) (catalog.CommercePolicy, error) {
	return catalog.CommercePolicy{}, catalog.ErrNoPolicy
}
func (noOfferings) Reserve(context.Context, uuid.UUID, int, uuid.UUID) error {
	return errors.New("no catalog")
}

// Payment fixtures insert their orders directly, so there is no reserved inventory to release.
func (noOfferings) Release(context.Context, uuid.UUID, int) error { return nil }

type noParties struct{}

func (noParties) Get(context.Context, uuid.UUID) (parties.LegalParty, error) {
	return parties.LegalParty{}, parties.ErrNotFound
}

type noPayees struct{}

func (noPayees) DefaultFor(context.Context, uuid.UUID) (parties.Payee, bool, error) {
	return parties.Payee{}, false, nil
}
func (noPayees) IsActive(context.Context, uuid.UUID) (bool, error) { return false, nil }

// ---- the environment

// lineParents are the parent rows of an order line (offering, service,
// policy, seller org and payee; PostgreSQL checks them, memory does not).
type lineParents struct {
	offering, service, policy, seller uuid.UUID
	payee                             *uuid.UUID
}

// backend is where the env keeps its rows: the in-memory stores, or
// PostgreSQL (store_integration_test.go).
type backend struct {
	tx       db.TxManager
	store    payments.Store
	ordStore orders.Store
	entStore entitlements.Store
	ledStore ledger.Store
	idem     idempotency.Store
	// booth and platform return the parents of a booth line (club_a's
	// payee) and of Koro's platform-native line.
	booth, platform func(t *testing.T) lineParents
	// fileAdded makes an upload exist where the rows are (PostgreSQL FKs).
	fileAdded func(t *testing.T, fid uuid.UUID, purpose files.Purpose)
}

func memBackend(clk *clock.Fake) backend {
	payee := id("payee_club_a")
	return backend{
		tx: db.NopTx{}, store: payments.NewMemStore(clk.Now), ordStore: orders.NewMemStore(clk.Now),
		entStore: entitlements.NewMemStore(clk.Now), ledStore: ledger.NewMemStore(clk.Now), idem: idempotency.NewMemStore(),
		booth: func(*testing.T) lineParents {
			return lineParents{offering: uuid.NewV7(), service: uuid.NewV7(), policy: uuid.NewV7(), seller: id("club_a"), payee: &payee}
		},
		platform: func(*testing.T) lineParents {
			return lineParents{offering: uuid.NewV7(), service: uuid.NewV7(), policy: uuid.NewV7(), seller: id("koro_platform")}
		},
	}
}

// env is payments with the real orders (orders and entitlements stores)
// and ledger modules over one backend.
type env struct {
	clk      *clock.Fake
	b        backend
	store    payments.Store
	mem      *payments.MemStore // nil on PostgreSQL
	ordStore orders.Store
	entStore entitlements.Store
	ents     entitlements.Service
	ordersM  orders.Module
	ledStore ledger.Store
	ledger   ledger.Service
	ledMod   ledger.Module
	audit    *audittest.Recorder
	outbox   *outbox.Mem
	queue    *queue.Mem
	files    *fakeFiles
	sender   *fakeSender
	keeper   *idempotency.Keeper
	platform uuid.UUID
	mod      payments.Module
	svc      payments.Service
}

type envOption func(*payments.Deps)

func withPoster(wrap func(payments.Poster) payments.Poster) envOption {
	return func(d *payments.Deps) { d.Ledger = wrap(d.Ledger) }
}

// withLedgerLines wraps the ledger reads of a line refund (BE-043).
func withLedgerLines(wrap func(payments.LedgerLines) payments.LedgerLines) envOption {
	return func(d *payments.Deps) { d.LedgerLines = wrap(d.LedgerLines) }
}

// withOrders wraps payments' orders port.
func withOrders(wrap func(payments.Orders) payments.Orders) envOption {
	return func(d *payments.Deps) { d.Orders = wrap(d.Orders) }
}

// withOutbox wraps payments' outbox writer.
func withOutbox(wrap func(outbox.Writer) outbox.Writer) envOption {
	return func(d *payments.Deps) { d.Outbox = wrap(d.Outbox) }
}

// newEnv is the in-memory env.
func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	return newEnvOn(t, clk, memBackend(clk), id("koro_platform_party"), opts...)
}

func newEnvOn(t *testing.T, clk *clock.Fake, b backend, platform uuid.UUID, opts ...envOption) *env {
	t.Helper()
	e := &env{clk: clk, b: b, store: b.store, ordStore: b.ordStore, entStore: b.entStore, ledStore: b.ledStore, audit: &audittest.Recorder{},
		outbox: outbox.NewMem(clk), queue: queue.NewMem(), files: &fakeFiles{m: map[uuid.UUID]files.FileMeta{}}, sender: &fakeSender{},
		platform: platform}
	e.mem, _ = b.store.(*payments.MemStore)
	if b.fileAdded != nil {
		e.files.added = func(fid uuid.UUID, purpose files.Purpose) { b.fileAdded(t, fid, purpose) }
	}
	e.keeper = idempotency.New(idempotency.Deps{Tx: b.tx, Store: b.idem, Clock: clk, Log: discardLog()})
	ents, err := entitlements.New(entitlements.Deps{Store: e.entStore, Tx: b.tx, Audit: e.audit, Idempotency: e.keeper, Clock: clk, Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	e.ents = ents.Service
	if e.ordersM, err = orders.New(orders.Deps{Store: e.ordStore, Tx: b.tx, Audit: e.audit, Idempotency: e.keeper, Orgs: noOrgs{},
		Offerings: noOfferings{}, Parties: noParties{}, Payees: noPayees{}, Entitlements: e.ents, Outbox: e.outbox, Clock: clk,
		Log: discardLog()}); err != nil {
		t.Fatal(err)
	}
	// The ledger reads the settled lines through the real orders module
	// (the allocation check and the late recognition, BE-067).
	led, err := ledger.New(ledger.Deps{Store: e.ledStore, Tx: b.tx, Clock: clk, Log: discardLog(), Orders: e.ordersM.Orders})
	if err != nil {
		t.Fatal(err)
	}
	e.ledger, e.ledMod = led.Service, led
	d := payments.Deps{
		Store: e.store, Tx: b.tx, Audit: e.audit, Idempotency: e.keeper, Orders: e.ordersM.Orders, Ledger: e.ledger, Outbox: e.outbox,
		Queue: e.queue, Clock: clk, Log: discardLog(), Parties: fakeParties{platform: e.platform}, Files: e.files, Reauth: fakeReauth{},
		Orgs: fakeOrgs{}, Sender: e.sender, Users: fakeUsers{}, Staff: fakeStaff{}, LedgerLines: led,
	}
	for _, o := range opts {
		o(&d)
	}
	if e.mod, err = payments.New(d); err != nil {
		t.Fatal(err)
	}
	e.svc = e.mod.Service
	return e
}

// activeAccount inserts an approved account (created by finance_admin_2,
// approved by platform_admin_2); def makes it the default.
func (e *env) activeAccount(t *testing.T, number string, def bool) payments.CollectionAccount {
	t.Helper()
	ctx := t.Context()
	c, err := e.store.InsertAccount(ctx, payments.CollectionAccount{ID: uuid.NewV7(), Label: "Ana hesap", LegalPartyID: e.platform,
		IBAN: number, HolderName: "Koro Teknoloji A.Ş.", BankName: "Örnek Bank", Status: payments.AccountPending,
		CreatedBy: id("finance_admin_2"), CreatedAt: e.clk.Now()})
	if err != nil {
		t.Fatal(err)
	}
	by, at := id("platform_admin_2"), e.clk.Now()
	c.Status, c.ApprovedBy, c.ApprovedAt, c.IsDefault = payments.AccountActive, &by, &at, def
	if c, err = e.store.UpdateAccount(ctx, c, payments.AccountPending); err != nil {
		t.Fatal(err)
	}
	return c
}

// transfer records a transfer of amount with description desc on acct as
// finance_admin_2 (sender IBAN when sender is set).
func (e *env) transfer(t *testing.T, acct payments.CollectionAccount, amount int64, desc string, sender bool) payments.IncomingTransfer {
	t.Helper()
	a := actor(t, "finance_admin_2")
	in := payments.TransferInput{CollectionAccountID: acct.ID, ValueDate: civil.From(e.clk.Now()), Amount: amount,
		Description: desc, BankReference: fmt.Sprintf("REF-%d", seq.Add(1)), SenderName: "ÖRNEK TEKNOLOJİ A.Ş."}
	if sender {
		in.SenderIBAN = ibanSender
	}
	tr, err := e.svc.CreateTransfer(ctxOf(t, a), a, in)
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	return tr
}

var seq atomic.Int64

// orderSpec is an order of company_a: a booth line of club_a's payee
// (1,000,000 + 20 % VAT, 10 % commission from the seller), optionally a
// platform-native line (Koro's targeted posts, 250,000 + VAT, pending
// right) and the model C variant (fee from the buyer, paid directly).
type orderSpec struct {
	platformLine bool
	modelC       bool
	paidFee      bool // model C: the fee is already collected
	// platformOnly: Koro's platform-native line alone, no booth (BE-067).
	platformOnly bool
	// withheld: the platform line's VAT is withheld 5/10 by the buyer
	// (tevkifat; 25,000 of its 50,000 VAT; BE-067).
	withheld bool
}

type placed struct {
	order orders.Order
	lines []orders.Line
}

func (e *env) order(t *testing.T, spec orderSpec) placed {
	t.Helper()
	now := e.clk.Now()
	o := orders.Order{ID: uuid.NewV7(), OrderNo: fmt.Sprintf("KR%08d", seq.Add(1)), OrgID: id("company_a"), OrgKind: "company",
		PlacedByUserID: id("company_admin"), BuyerPartyID: id("party_buyer_a"), BuyerSnapshot: orders.BuyerSnapshot{Title: "Örnek A.Ş."},
		ModelCode: orders.ModelA, CollectionMethod: orders.CollectBankTransfer, Status: orders.StatusAwaitingPayment, Currency: "TRY",
		ExpiresAt: now.Add(7 * 24 * time.Hour), PlacedAt: now, CreatedAt: now, UpdatedAt: now}
	bp := e.b.booth(t)
	booth := orders.Line{ID: uuid.NewV7(), OrderID: o.ID, OrgID: o.OrgID, SellerOrgID: bp.seller, PayeeID: bp.payee,
		OfferingID: bp.offering, ServiceID: bp.service, CommercePolicyID: bp.policy, TitleSnapshot: "Stant", Quantity: 1,
		UnitPriceAmount: 1_000_000, NetAmount: 1_000_000, VATRateBP: 2000, VATAmount: 200_000, GrossAmount: 1_200_000,
		CommissionRateBP: 1000, CommissionBasis: "net_excl_vat", CommissionPayer: "seller", CommissionAmount: 100_000,
		CommissionVATRateBP: 2000, CommissionVATAmount: 20_000, DisputeWindowDays: 7, SellerNetAmount: 1_080_000,
		PayableAmount: 1_200_000, FulfillmentStatus: orders.FulfillPending, CreatedAt: now, UpdatedAt: now}
	if spec.modelC {
		o.ModelCode, o.CollectionMethod = orders.ModelC, orders.CollectDirectToPayee
		booth.CommissionPayer, booth.BuyerFeeAmount, booth.SellerNetAmount, booth.PayableAmount = "buyer", 120_000, 1_200_000, 1_320_000
	}
	lines := []orders.Line{booth}
	if spec.platformOnly {
		lines, spec.platformLine = nil, true
	}
	if spec.platformLine {
		pp := e.b.platform(t)
		pl := orders.Line{ID: uuid.NewV7(), OrderID: o.ID, OrgID: o.OrgID, SellerOrgID: pp.seller,
			OfferingID: pp.offering, ServiceID: pp.service, CommercePolicyID: pp.policy, TitleSnapshot: "5 hedefli gönderi",
			Quantity: 1, UnitPriceAmount: 250_000, NetAmount: 250_000, VATRateBP: 2000, VATAmount: 50_000, GrossAmount: 300_000,
			CommissionBasis: "net_excl_vat", CommissionPayer: "seller", CommissionVATRateBP: 2000, DisputeWindowDays: 7,
			SellerNetAmount: 300_000, PayableAmount: 300_000, FulfillmentStatus: orders.FulfillNotApplicable, CreatedAt: now, UpdatedAt: now}
		if spec.withheld {
			num, den := int16(5), int16(10)
			pl.VATWithholdingNum, pl.VATWithholdingDen, pl.VATWithheldAmount = &num, &den, 25_000
			pl.SellerNetAmount, pl.PayableAmount = 275_000, 275_000
		}
		lines = append(lines, pl)
	}
	for _, l := range lines {
		o.SubtotalAmount += l.NetAmount
		o.VATAmount += l.VATAmount
		o.VATWithheldAmount += l.VATWithheldAmount
		o.BuyerFeeAmount += l.BuyerFeeAmount
		o.TotalAmount += l.GrossAmount + l.BuyerFeeAmount
		o.PayableAmount += l.PayableAmount
	}
	o.KoroCollectAmount = o.PayableAmount
	if spec.modelC {
		o.KoroCollectAmount, o.DirectToPayeeAmount = o.BuyerFeeAmount, o.PayableAmount-o.BuyerFeeAmount
		if spec.paidFee {
			o.PaidAmount = o.KoroCollectAmount
		}
	}
	err := e.b.tx.WithTx(t.Context(), func(ctx context.Context) error { return e.ordStore.InsertOrder(ctx, o, lines) })
	if err != nil {
		t.Fatal(err)
	}
	if spec.platformLine {
		q, src := 5, lines[len(lines)-1].ID
		if _, err := e.ents.Grant(t.Context(), entitlements.GrantInput{OrgID: o.OrgID, Feature: entitlements.FeatureTargetedPost,
			Quantity: &q, ValidFrom: now, ValidTo: now.AddDate(0, 3, 0), Status: entitlements.StatusPending,
			SourceType: entitlements.SourceOrderLine, SourceID: &src}); err != nil {
			t.Fatal(err)
		}
	}
	return placed{order: o, lines: lines}
}

// match matches tr to p as finance_admin.
func (e *env) match(t *testing.T, tr payments.IncomingTransfer, p placed, refundIBAN string) (payments.MatchResult, error) {
	t.Helper()
	a := actor(t, "finance_admin")
	return e.matchCtx(ctxOf(t, a), tr, p, refundIBAN)
}

// matchCtx matches tr to p as the actor of ctx (an Idempotency-Key in ctx).
func (e *env) matchCtx(ctx context.Context, tr payments.IncomingTransfer, p placed, refundIBAN string) (payments.MatchResult, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return payments.MatchResult{}, err
	}
	return e.svc.MatchTransfer(ctx, a, tr.ID, payments.MatchInput{OrderID: p.order.ID, ExpectedOrderNo: p.order.OrderNo, RefundIBAN: refundIBAN})
}

// events returns the outbox events of a topic.
func (e *env) events(topic string) []domainevent.Envelope {
	var out []domainevent.Envelope
	for _, ev := range e.outbox.Events() {
		if ev.Topic == topic {
			out = append(out, ev)
		}
	}
	return out
}

// deliver runs the payments subscriptions of topic for every event of it.
func (e *env) deliver(t *testing.T, topic string) {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	for _, ev := range e.events(topic) {
		for _, s := range r.For(topic) {
			if err := s.Handle(t.Context(), ev); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
}

// journal returns every ledger transaction (newest first).
func (e *env) journal(t *testing.T) []ledger.Posted {
	t.Helper()
	ps, err := e.ledStore.List(t.Context(), ledger.JournalFilter{}, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// balances sums the journal per account (payee accounts per payee).
func balances(ps []ledger.Posted) map[string]int64 {
	out := map[string]int64{}
	for _, p := range ps {
		for _, en := range p.Entries {
			k := string(en.Account)
			if en.PayeeID != nil {
				k += ":" + en.PayeeID.String()
			}
			out[k] += en.Amount
		}
	}
	return out
}

func wantErrIs(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func containsAll(got []uuid.UUID, want ...uuid.UUID) bool {
	for _, w := range want {
		if !slices.Contains(got, w) {
			return false
		}
	}
	return true
}

func (noPayees) Get(context.Context, uuid.UUID) (parties.Payee, error) {
	return parties.Payee{}, parties.ErrNotFound
}
func (noPayees) ApprovedAccount(context.Context, uuid.UUID) (parties.BankAccount, error) {
	return parties.BankAccount{}, parties.ErrNoApprovedAccount
}
