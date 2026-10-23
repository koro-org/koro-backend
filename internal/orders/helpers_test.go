package orders_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/catalog"
	"koro/internal/entitlements"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

func org(name string) uuid.UUID { return authztest.ID(name) }

func ptr[T any](v T) *T { return &v }

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// auditLog records the events a service writes.
type auditLog struct {
	mu     sync.Mutex
	events []audit.Event
}

func (l *auditLog) Log(_ context.Context, e audit.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return nil
}

func (l *auditLog) of(act authz.Action) []audit.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []audit.Event
	for _, e := range l.events {
		if e.Action == string(act) {
			out = append(out, e)
		}
	}
	return out
}

// fakeOrgs is orgs.OrgDirectory over the matrix orgs (authztest.Orgs).
type fakeOrgs struct {
	mu sync.Mutex
	m  map[uuid.UUID]orgs.OrgSummary
}

func newFakeOrgs() *fakeOrgs {
	f := &fakeOrgs{m: map[uuid.UUID]orgs.OrgSummary{}}
	for _, o := range authztest.Orgs() {
		id := authztest.ID(o.Name)
		f.m[id] = orgs.OrgSummary{ID: id, Kind: orgs.Kind(o.Kind), Name: "Org " + o.Name, Status: orgs.StatusActive, Slug: o.Name}
	}
	return f
}

func (f *fakeOrgs) setStatus(name string, st orgs.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.m[org(name)]
	o.Status = st
	f.m[org(name)] = o
}

func (f *fakeOrgs) Get(_ context.Context, id uuid.UUID) (orgs.OrgSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.m[id]
	if !ok {
		return orgs.OrgSummary{}, orgs.ErrNotFound
	}
	return o, nil
}

func (f *fakeOrgs) GetMany(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID]orgs.OrgSummary{}
	for _, id := range ids {
		if o, ok := f.m[id]; ok {
			out[id] = o
		}
	}
	return out, nil
}

func (f *fakeOrgs) ActiveMemberIDs(_ context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.Actor.MemberOf(orgID) {
			out = append(out, p.User())
		}
	}
	return out, nil
}

func (f *fakeOrgs) HasRole(_ context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error) {
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.User() == userID && p.Actor.HasOrgRole(orgID, roles...) {
			return true, nil
		}
	}
	return false, nil
}

// fakeParties holds legal parties by ID.
type fakeParties struct {
	mu sync.Mutex
	m  map[uuid.UUID]parties.LegalParty
}

func (f *fakeParties) add(p parties.LegalParty) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.ID == uuid.Nil() {
		p.ID = uuid.NewV7()
	}
	f.m[p.ID] = p
	return p.ID
}

func (f *fakeParties) Get(_ context.Context, id uuid.UUID) (parties.LegalParty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.m[id]
	if !ok {
		return parties.LegalParty{}, parties.ErrNotFound
	}
	return p, nil
}

// buyerParty is a verified buyer_company party of the company org.
func buyerParty(companyOrg string, st parties.Status) parties.LegalParty {
	o := org(companyOrg)
	return parties.LegalParty{Kind: parties.KindBuyerCompany, LegalType: parties.LegalCompany, OrgID: &o,
		Title: "Örnek Teknoloji A.Ş.", TaxOffice: ptr("Kozyatağı"), TaxNo: ptr("1234567890"), Email: ptr("fatura@example.test"),
		Address: parties.Address{Line1: "Atatürk Cad. 1", City: "İstanbul"}, Status: st}
}

// fakePayees holds payees.
type fakePayees struct {
	mu       sync.Mutex
	m        map[uuid.UUID]parties.Payee
	accounts map[uuid.UUID]parties.BankAccount
}

func (f *fakePayees) add(orgName string, st parties.PayeeStatus) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewV7()
	f.m[id] = parties.Payee{ID: id, OrgID: org(orgName), Status: st, IsDefault: true, ValidFrom: t0.Add(-time.Hour)}
	return id
}

func (f *fakePayees) set(id uuid.UUID, st parties.PayeeStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.m[id]
	p.Status = st
	f.m[id] = p
}

func (f *fakePayees) DefaultFor(_ context.Context, orgID uuid.UUID) (parties.Payee, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.m {
		if p.OrgID == orgID && p.IsDefault && p.Status == parties.PayeeActive {
			return p, true, nil
		}
	}
	return parties.Payee{}, false, nil
}

func (f *fakePayees) IsActive(_ context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.m[id]
	if !ok {
		return false, parties.ErrNotFound
	}
	return p.Status == parties.PayeeActive, nil
}

func (f *fakePayees) Get(_ context.Context, id uuid.UUID) (parties.Payee, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.m[id]
	if !ok {
		return parties.Payee{}, parties.ErrNotFound
	}
	return p, nil
}

func (f *fakePayees) ApprovedAccount(_ context.Context, id uuid.UUID) (parties.BankAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[id]
	if !ok {
		return parties.BankAccount{}, parties.ErrNoApprovedAccount
	}
	return a, nil
}

// fakeSender records the notices (deduplicated by key).
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

func (f *fakeSender) all() []notify.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.Message(nil), f.sent...)
}

// fakeUsers resolves every persona to an address of its name.
type fakeUsers struct{}

func (fakeUsers) Contact(_ context.Context, id uuid.UUID) (identity.UserContact, error) {
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.User() == id && p.Actor.IsUser() {
			return identity.UserContact{ID: id, Email: p.Name + "@koro.test", FirstName: p.Name, Locale: "tr"}, nil
		}
	}
	return identity.UserContact{}, identity.ErrNotFound
}

// fakeStaff: finance_admin and finance_admin_2.
type fakeStaff struct{}

func (fakeStaff) FinanceAdmins(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{authztest.ID("finance_admin"), authztest.ID("finance_admin_2")}, nil
}

// env is an orders module over in-memory stores, with the real catalog and
// entitlements modules underneath (their in-memory stores).
type env struct {
	clk      *clock.Fake
	store    *orders.MemStore
	cat      *catalog.MemStore
	catalog  catalog.Module
	entStore entitlements.Store
	ents     entitlements.Service
	audit    *auditLog
	orgs     *fakeOrgs
	parties  *fakeParties
	payees   *fakePayees
	outbox   *outbox.Mem
	sender   *fakeSender
	acct     *orders.CollectionAccount // nil: no active collection account
	svc      orders.Service
	mod      orders.Module

	// The real files module over in-memory stores (the delivery evidence, BE-093).
	files     files.Service
	fileStore files.Store
	objects   *storage.Mem

	booth, quota uuid.UUID // a reviewed org-bound service and a platform-native one (5 targeted posts per unit)
	policy       catalog.CommercePolicy
	payeeA       uuid.UUID // club_a's active default payee
	partyA       uuid.UUID // company_a's verified buyer party
}

// evidenceBuckets are the buckets of the files module of the tests.
var evidenceBuckets = files.Buckets{Files: "files-test", Quarantine: "quarantine-test"}

// newEnv builds the env; opts edit the orders dependencies before New.
func newEnv(t *testing.T, opts ...func(*orders.Deps)) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	e := &env{clk: clk, store: orders.NewMemStore(clk.Now), cat: catalog.NewMemStore(clk.Now), audit: &auditLog{},
		orgs: newFakeOrgs(), parties: &fakeParties{m: map[uuid.UUID]parties.LegalParty{}},
		payees: &fakePayees{m: map[uuid.UUID]parties.Payee{}}, outbox: outbox.NewMem(clk), sender: &fakeSender{},
		fileStore: files.NewMemStore(clk.Now), objects: storage.NewMem(clk)}
	tx := db.NopTx{}
	keeper := idempotency.New(idempotency.Deps{Tx: tx, Store: idempotency.NewMemStore(), Clock: clk, Log: discardLog()})
	fm, err := files.New(files.Deps{Store: e.fileStore, Objects: e.objects, Tx: tx, Outbox: e.outbox, Queue: queue.NewMem(),
		Idempotency: keeper, Clock: clk, Log: discardLog(), Buckets: evidenceBuckets, Audit: e.audit})
	if err != nil {
		t.Fatalf("files.New: %v", err)
	}
	e.files = fm.Service
	var held catalog.BuyerHeld = func(ctx context.Context, offeringID, buyer uuid.UUID) (int, error) {
		return e.mod.BuyerHeld(ctx, offeringID, buyer)
	}
	cat, err := catalog.New(catalog.Deps{Store: e.cat, Tx: tx, Audit: e.audit, Idempotency: keeper, Orgs: e.orgs,
		Outbox: e.outbox, Queue: queue.NewMem(), Clock: clk, Log: discardLog(), BuyerHeld: held})
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	e.catalog = cat
	e.entStore = entitlements.NewMemStore(clk.Now)
	ents, err := entitlements.New(entitlements.Deps{Store: e.entStore, Tx: tx, Audit: e.audit, Idempotency: keeper, Clock: clk, Log: discardLog()})
	if err != nil {
		t.Fatalf("entitlements.New: %v", err)
	}
	e.ents = ents.Service
	d := orders.Deps{
		Store: e.store, Tx: tx, Audit: e.audit, Idempotency: keeper, Orgs: e.orgs, Offerings: cat.Offerings, Parties: e.parties,
		Payees: e.payees, Entitlements: e.ents, Outbox: e.outbox, Clock: clk, Log: discardLog(),
		CollectionAccount: func(context.Context) (orders.CollectionAccount, bool, error) {
			if e.acct == nil {
				return orders.CollectionAccount{}, false, nil
			}
			return *e.acct, true, nil
		},
		Sender: e.sender, Users: fakeUsers{}, Staff: fakeStaff{}, Files: e.files,
	}
	for _, o := range opts {
		o(&d)
	}
	mod, err := orders.New(d)
	if err != nil {
		t.Fatalf("orders.New: %v", err)
	}
	e.mod, e.svc = mod, mod.Service
	e.booth = e.service(t, "booth_stand", catalog.FulfillOrgBound, nil)
	e.quota = e.service(t, "targeted_post_5", catalog.FulfillPlatformNative, func(s *catalog.ServiceItem) {
		s.Category = catalog.CategoryTargetedPostQuota
		s.EntitlementTemplate = catalog.JSON(`{"feature":"targeted_post","kind":"quota","quantity_per_unit":5,"valid_days":90}`)
	})
	e.payeeA = e.payees.add("club_a", parties.PayeeActive)
	e.policy = e.addPolicy(t, catalog.ScopeDefault, nil, nil)
	e.partyA = e.parties.add(buyerParty("company_a", parties.StatusVerified))
	return e
}

// service inserts a reviewed, active service.
func (e *env) service(t *testing.T, code string, ft catalog.FulfillmentType, mut func(*catalog.ServiceItem)) uuid.UUID {
	t.Helper()
	at := t0.Add(-24 * time.Hour)
	v := catalog.ServiceItem{ID: uuid.NewV7(), Code: code, NameTR: "Hizmet " + code, Category: catalog.CategoryBooth,
		FulfillmentType: ft, DefaultVATRateBP: 2000, VATClass: catalog.VATStandard, AccountingReviewedAt: &at, Active: true}
	if mut != nil {
		mut(&v)
	}
	got, err := e.cat.InsertService(t.Context(), v)
	if err != nil {
		t.Fatalf("InsertService: %v", err)
	}
	return got.ID
}

// addPolicy inserts a policy (default: model A, 10 % from the seller, net
// of VAT, a 7-day dispute window) of scope, valid from a day before t0.
func (e *env) addPolicy(t *testing.T, scope catalog.PolicyScope, scopeID *uuid.UUID, mut func(*catalog.CommercePolicy)) catalog.CommercePolicy {
	t.Helper()
	p := catalog.CommercePolicy{ID: uuid.NewV7(), Scope: scope, ScopeID: scopeID, ModelCode: catalog.ModelA,
		CollectionMethod: catalog.CollectBankTransfer, CommissionRateBP: 1000, CommissionBasis: catalog.BasisNetExclVAT,
		CommissionPayer: catalog.PayerSeller, CommissionVATRateBP: 2000, InvoiceAutomation: catalog.InvoiceManual,
		DisputeWindowDays: 7, PayoutHoldDays: 0, ValidFrom: t0.Add(-24 * time.Hour), CreatedBy: authztest.ID("finance_admin_2")}
	if mut != nil {
		mut(&p)
	}
	got, err := e.cat.InsertPolicy(t.Context(), p)
	if err != nil {
		t.Fatalf("InsertPolicy: %v", err)
	}
	return got
}

// offering inserts an active offering of club_a (payee A, 1,000,000 kuruş
// excluding 20 % VAT, a stock of 5, no per-buyer limit).
func (e *env) offering(t *testing.T, mut func(*catalog.Offering)) uuid.UUID {
	t.Helper()
	at := t0.Add(-24 * time.Hour)
	o := catalog.Offering{ID: uuid.NewV7(), ServiceID: e.booth, SellerKind: catalog.SellerOrg, OrgID: org("club_a"),
		DefaultPayeeID: &e.payeeA, Title: "Bahar Şenliği Stant Paketi",
		Deliverables:    catalog.Deliverables{{Key: "booth_3x3", Label: "3x3 stant alanı"}},
		UnitPriceAmount: 1_000_000, Currency: "TRY", VATRateBP: 2000, CompensationType: "cash", QuantityTotal: ptr(5),
		InvoiceTrigger: catalog.InvoiceOnFulfillment, UniversityApprovalStatus: catalog.UniNotRequired, Status: catalog.OfferingActive,
		SubmittedAt: &at, ApprovedBy: ptr(authztest.ID("platform_admin_2")), ApprovedAt: &at, CreatedBy: authztest.ID("club_admin")}
	if mut != nil {
		mut(&o)
	}
	got, err := e.cat.InsertOffering(t.Context(), o)
	if err != nil {
		t.Fatalf("InsertOffering: %v", err)
	}
	return got.ID
}

// platformOffering is an active offering of the platform org selling the
// platform-native quota service (no payee).
func (e *env) platformOffering(t *testing.T) uuid.UUID {
	return e.offering(t, func(o *catalog.Offering) {
		o.ServiceID, o.SellerKind, o.OrgID, o.DefaultPayeeID = e.quota, catalog.SellerPlatform, org("platform_org"), nil
		o.Title, o.UnitPriceAmount, o.QuantityTotal = "5 hedefli gönderi", 250_000, nil
	})
}

func (e *env) sold(t *testing.T, offeringID uuid.UUID) int {
	t.Helper()
	o, err := e.cat.GetOffering(t.Context(), offeringID, false)
	if err != nil {
		t.Fatal(err)
	}
	return o.QuantitySold
}

func actor(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func ctxOf(t *testing.T, a authz.Actor) context.Context { return authz.WithActor(t.Context(), a) }

// place orders the lines as company_admin for company_a, billed to party A.
func (e *env) place(t *testing.T, lines ...orders.LineInput) []orders.OrderView {
	t.Helper()
	a := actor(t, "company_admin")
	vs, err := e.svc.CreateOrder(ctxOf(t, a), a, org("company_a"), orders.CreateInput{BuyerPartyID: e.partyA, Lines: lines})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	return vs
}

func line(offering uuid.UUID, qty int) orders.LineInput {
	return orders.LineInput{OfferingID: offering, Quantity: qty}
}

// placeOne places one line of a fresh offering and returns the order.
func (e *env) placeOne(t *testing.T) orders.OrderView {
	t.Helper()
	return e.place(t, line(e.offering(t, nil), 1))[0]
}

// pay applies the whole amount due of the order.
func (e *env) pay(t *testing.T, id uuid.UUID) orders.PaymentEffect {
	t.Helper()
	p, err := e.mod.Orders.ForPayment(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	eff, err := e.mod.Orders.ApplyPayment(t.Context(), id, uuid.NewV7(), p.DueAmount)
	if err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	return eff
}

func (e *env) order(t *testing.T, id uuid.UUID) orders.Order {
	t.Helper()
	o, err := e.store.GetOrder(t.Context(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (e *env) line(t *testing.T, id uuid.UUID) orders.Line {
	t.Helper()
	l, err := e.store.GetLine(t.Context(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// rights returns the entitlements granted by a line.
func (e *env) rights(t *testing.T, lineID uuid.UUID) []entitlements.Entitlement {
	t.Helper()
	es, err := e.entStore.BySource(t.Context(), entitlements.SourceOrderLine, lineID)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// topics returns the outbox topics written so far with their aggregate.
func (e *env) events(topic string) []domainevent.Envelope {
	var out []domainevent.Envelope
	for _, ev := range e.outbox.Events() {
		if ev.Topic == topic {
			out = append(out, ev)
		}
	}
	return out
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func problemOf(err error) (int, string) {
	if p, ok := errors.AsType[*problem.Error](err); ok {
		return p.Status(), p.Code()
	}
	return 0, ""
}

func wantProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	if s, c := problemOf(err); s != status || c != code {
		t.Fatalf("error = %v (%d %s), want %d %s", err, s, c, status, code)
	}
}

// ---- HTTP

func newHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: discardLog(),
		Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(idempotency.Middleware(app.API))
	orders.Register(app.V1, e.svc)
	return app
}

// call sends a request as persona with headers and decodes the JSON answer.
func call(t *testing.T, app *httpx.App, persona, method, path string, body any, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if s, ok := body.(string); ok {
			buf.WriteString(s)
		} else if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if p, _ := authztest.Get(persona); p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}
