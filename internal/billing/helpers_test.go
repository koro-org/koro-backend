package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/billing"
	"koro/internal/catalog"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

// reauthPassword is the proof the fake re-authenticator accepts (not a
// credential of anything).
const reauthPassword = "test-reauth-password-not-real"

// pdfBytes is the content the fake files serve.
var pdfBytes = []byte("%PDF-1.7\n% billing test invoice\n%%EOF\n")

func actor(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func ctxOf(t *testing.T, a authz.Actor) context.Context { return authz.WithActor(t.Context(), a) }

func ptr[T any](v T) *T { return &v }

// wantProblem fails unless err is (or wraps) want.
func wantProblem(t *testing.T, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	p, ok := errors.AsType[*problem.Error](err)
	if !ok || p.Status() != status {
		t.Fatalf("error = %v, want HTTP %d", err, status)
	}
}

// ---- fakes

// fakeOrders holds orders with their lines.
type fakeOrders struct {
	mu     sync.Mutex
	orders map[uuid.UUID]orders.Order
	lines  map[uuid.UUID][]orders.Line
}

func (f *fakeOrders) ForBilling(_ context.Context, id uuid.UUID) (orders.Order, []orders.Line, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.orders[id]
	if !ok {
		return orders.Order{}, nil, orders.ErrNotFound
	}
	return o, slices.Clone(f.lines[id]), nil
}

func (f *fakeOrders) PayoutLines(_ context.Context, payoutID uuid.UUID) ([]orders.Line, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []orders.Line
	for _, ls := range f.lines {
		for _, l := range ls {
			if l.PayoutID != nil && *l.PayoutID == payoutID {
				out = append(out, l)
			}
		}
	}
	slices.SortFunc(out, func(a, b orders.Line) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return out, nil
}

// update changes a stored line.
func (f *fakeOrders) update(id uuid.UUID, fn func(*orders.Line)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for o, ls := range f.lines {
		for i := range ls {
			if ls[i].ID == id {
				fn(&f.lines[o][i])
			}
		}
	}
}

// fakeTerms answers the invoice terms of an offering (on_fulfillment by
// default, manual automation).
type fakeTerms struct {
	mu       sync.Mutex
	triggers map[uuid.UUID]catalog.InvoiceTrigger
}

func (f *fakeTerms) InvoiceTerms(_ context.Context, offeringID, _ uuid.UUID) (catalog.InvoiceTerms, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tr, ok := f.triggers[offeringID]
	if !ok {
		tr = catalog.InvoiceOnFulfillment
	}
	return catalog.InvoiceTerms{OfferingID: offeringID, Trigger: tr, Automation: catalog.InvoiceManual}, nil
}

// fakeParties holds the legal parties, Koro's own and the payees.
type fakeParties struct {
	mu       sync.Mutex
	parties  map[uuid.UUID]parties.LegalParty
	payees   map[uuid.UUID]parties.Payee
	platform *uuid.UUID
}

func (f *fakeParties) Get(_ context.Context, id uuid.UUID) (parties.LegalParty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.parties[id]
	if !ok {
		return parties.LegalParty{}, parties.ErrNotFound
	}
	return p, nil
}

func (f *fakeParties) Platform(ctx context.Context) (parties.LegalParty, error) {
	f.mu.Lock()
	id := f.platform
	f.mu.Unlock()
	if id == nil {
		return parties.LegalParty{}, parties.ErrNotFound
	}
	return f.Get(ctx, *id)
}

// fakePayees reads the payees of fakeParties.
type fakePayees struct{ p *fakeParties }

func (f fakePayees) Get(_ context.Context, id uuid.UUID) (parties.Payee, error) {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	py, ok := f.p.payees[id]
	if !ok {
		return parties.Payee{}, parties.ErrNotFound
	}
	return py, nil
}

// fakeOrgs is the org directory over the matrix orgs and personas.
type fakeOrgs struct{}

func (fakeOrgs) Get(_ context.Context, id uuid.UUID) (orgs.OrgSummary, error) {
	for _, o := range authztest.Orgs() {
		if authztest.ID(o.Name) == id {
			return orgs.OrgSummary{ID: id, Kind: orgs.Kind(o.Kind), Name: o.Name, Status: orgs.StatusActive, Slug: o.Name}, nil
		}
	}
	return orgs.OrgSummary{}, orgs.ErrNotFound
}

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

// fakeFiles holds file metadata; Attach records the owner; Read serves
// pdfBytes unless notClean.
type fakeFiles struct {
	mu       sync.Mutex
	meta     map[uuid.UUID]files.FileMeta
	attached map[uuid.UUID]files.Owner
	notClean bool
}

// put adds an active invoice_pdf PDF of uploader (and org when given).
func (f *fakeFiles) put(uploader string, org *uuid.UUID) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewV7()
	f.meta[id] = filesMeta(id, authztest.ID(uploader), org)
	return id
}

// filesMeta is an active, scanned (skipped) invoice_pdf PDF of by.
func filesMeta(id, by uuid.UUID, org *uuid.UUID) files.FileMeta {
	return files.FileMeta{ID: id, Purpose: files.PurposeInvoicePDF, UploadedBy: &by, OrgID: org, MIME: files.MIMEPDF,
		SizeBytes: int64(len(pdfBytes)), SHA256: make([]byte, 32), Status: files.StatusActive, ScanStatus: files.ScanSkipped,
		AttachedEntityID: uuid.Nil()}
}

func (f *fakeFiles) edit(id uuid.UUID, fn func(*files.FileMeta)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.meta[id]
	fn(&m)
	f.meta[id] = m
}

func (f *fakeFiles) Get(_ context.Context, id uuid.UUID) (files.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.meta[id]
	if !ok {
		return files.FileMeta{}, files.ErrNotFound
	}
	return m, nil
}

func (f *fakeFiles) Attach(_ context.Context, _ authz.Actor, id uuid.UUID, owner files.Owner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.meta[id]
	switch {
	case !ok:
		return files.ErrNotFound
	case m.Purpose != owner.Purpose:
		return files.ErrPurposeMismatch
	}
	if prev, ok := f.attached[id]; ok && prev != owner {
		return files.ErrAlreadyAttached
	}
	f.attached[id] = owner
	return nil
}

func (f *fakeFiles) PresignGet(_ context.Context, id uuid.UUID, ttl time.Duration) (files.Download, error) {
	return files.Download{URL: "https://files.test/" + id.String(), ExpiresAt: t0.Add(ttl)}, nil
}

func (f *fakeFiles) Read(_ context.Context, id uuid.UUID, _ int64) ([]byte, files.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.meta[id]
	switch {
	case !ok:
		return nil, files.FileMeta{}, files.ErrNotFound
	case f.notClean:
		return nil, files.FileMeta{}, files.ErrNotClean
	}
	return slices.Clone(pdfBytes), m, nil
}

// auditLog records the events.
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

// fakeReauth accepts reauthPassword.
type fakeReauth struct{}

func (fakeReauth) Verify(_ context.Context, _ uuid.UUID, p identity.ReauthProof) (string, error) {
	switch {
	case p.Password == "" && p.TOTPCode == "":
		return "", identity.ErrReauthRequired
	case p.Password != reauthPassword:
		return "", identity.ErrReauthFailed
	}
	return "password", nil
}

// fakeSender records the notices.
type fakeSender struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (s *fakeSender) Send(_ context.Context, m notify.Message) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
	return uuid.NewV7(), nil
}

func (s *fakeSender) all() []notify.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.msgs)
}

// fakeStaff lists finance_admin and finance_admin_2.
type fakeStaff struct{}

func (fakeStaff) FinanceAdmins(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{authztest.ID("finance_admin"), authztest.ID("finance_admin_2")}, nil
}

// testEnvelope seals the integration tokens (a fixed local test key).
func testEnvelope(t *testing.T) *crypto.Envelope {
	t.Helper()
	kek, err := crypto.NewLocalKEK([]crypto.Key{{ID: "test1", Secret: []byte("billing-test-kek-32-bytes-long!!")}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := crypto.NewEnvelope(kek)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// ---- env

type env struct {
	clk     *clock.Fake
	store   billing.Store
	orders  *fakeOrders
	terms   *fakeTerms
	parties *fakeParties
	files   *fakeFiles
	audit   *auditLog
	outbox  *outbox.Mem
	queue   *queue.Mem
	sender  *fakeSender
	reauth  *audittest.LateReauth[identity.ReauthProof] // fakeReauth with an overload switch
	sealer  *crypto.Envelope
	tx      db.TxManager
	mod     billing.Module
	svc     billing.Service
	subs    *outbox.Registry

	koro, buyer, payee, payeeParty uuid.UUID // Koro's party, company_a's buyer party, club_a's payee and its party
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	return newEnvWith(t, clk, billing.NewMemStore(clk.Now), db.NopTx{})
}

func newEnvWith(t *testing.T, clk *clock.Fake, store billing.Store, tx db.TxManager) *env {
	t.Helper()
	e := &env{
		clk: clk, store: store,
		orders:  &fakeOrders{orders: map[uuid.UUID]orders.Order{}, lines: map[uuid.UUID][]orders.Line{}},
		terms:   &fakeTerms{triggers: map[uuid.UUID]catalog.InvoiceTrigger{}},
		parties: &fakeParties{parties: map[uuid.UUID]parties.LegalParty{}, payees: map[uuid.UUID]parties.Payee{}},
		files:   &fakeFiles{meta: map[uuid.UUID]files.FileMeta{}, attached: map[uuid.UUID]files.Owner{}},
		audit:   &auditLog{}, outbox: outbox.NewMem(clk), queue: queue.NewMem(), sender: &fakeSender{},
		sealer: testEnvelope(t), subs: outbox.NewRegistry(),
		reauth: &audittest.LateReauth[identity.ReauthProof]{Next: fakeReauth{}},
	}
	e.tx = tx
	e.seedParties()
	mod, err := billing.New(e.deps(t))
	if err != nil {
		t.Fatalf("billing.New: %v", err)
	}
	e.mod, e.svc = mod, mod.Service
	mod.Subscribe(e.subs)
	if err := e.subs.Err(); err != nil {
		t.Fatal(err)
	}
	return e
}

// deps are the module's dependencies over e's fakes.
func (e *env) deps(t *testing.T) billing.Deps {
	t.Helper()
	keeper := idempotency.New(idempotency.Deps{Tx: e.tx, Store: idempotency.NewMemStore(), Clock: e.clk, Log: obs.Discard()})
	return billing.Deps{
		Store: e.store, Tx: e.tx, Clock: e.clk, Log: obs.Discard(), Audit: e.audit, Outbox: e.outbox, Queue: e.queue,
		Idempotency: keeper, Orders: e.orders, Offerings: e.terms, Parties: e.parties, Payees: fakePayees{p: e.parties},
		Orgs: fakeOrgs{}, Files: e.files, Reauth: e.reauth, Sealer: e.sealer, Sender: e.sender, Staff: fakeStaff{},
	}
}

// seedParties writes Koro's party, company_a's buyer party and club_a's
// payee (a foundation) with their invoice addresses.
func (e *env) seedParties() {
	e.koro, e.buyer, e.payee, e.payeeParty = uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	str := func(s string) *string { return &s }
	company := authztest.ID("company_a")
	e.parties.parties[e.koro] = parties.LegalParty{ID: e.koro, Kind: parties.KindPlatform, Title: "Koro Test A.Ş.", TaxNo: str("1111111111")}
	e.parties.parties[e.buyer] = parties.LegalParty{ID: e.buyer, Kind: parties.KindBuyerCompany, OrgID: &company,
		Title: "Alıcı Test A.Ş.", TaxNo: str("2222222222"), Email: str("fatura@alici.example.test")}
	e.parties.parties[e.payeeParty] = parties.LegalParty{ID: e.payeeParty, Kind: parties.KindPayee, Title: "Test Vakfı",
		TaxNo: str("3333333333"), Email: str("muhasebe@vakif.example.test")}
	e.parties.payees[e.payee] = parties.Payee{ID: e.payee, LegalPartyID: e.payeeParty, OrgID: authztest.ID("club_a"),
		Status: parties.PayeeActive}
	koro := e.koro
	e.parties.platform = &koro
}

// orderSpec builds a paid order of company_a: one line per spec.
type lineSpec struct {
	payee    bool                   // a seller line of club_a's payee (else platform-native)
	trigger  catalog.InvoiceTrigger // the offering's invoice trigger
	payer    string                 // commission payer (seller | buyer)
	fulfil   bool                   // fulfilled
	withheld int64                  // VAT withheld
}

// order writes a paid order of model with the lines and returns it.
func (e *env) order(t *testing.T, model string, specs ...lineSpec) (orders.Order, []orders.Line) {
	t.Helper()
	at := t0.Add(-48 * time.Hour)
	o := orders.Order{ID: uuid.NewV7(), OrderNo: "KR" + strings.ToUpper(uuid.NewV7().String()[:8]), OrgID: authztest.ID("company_a"),
		OrgKind: "company", PlacedByUserID: authztest.ID("company_admin"), BuyerPartyID: e.buyer,
		BuyerSnapshot: orders.BuyerSnapshot{Title: "Alıcı Test A.Ş.", TaxNo: "2222222222", Email: "eski@alici.example.test"},
		ModelCode:     model, CollectionMethod: orders.CollectBankTransfer, Status: orders.StatusPaid, Currency: "TRY",
		PlacedAt: at, PaidAt: &at, CreatedAt: at, UpdatedAt: at}
	if model == orders.ModelC {
		o.CollectionMethod = orders.CollectDirectToPayee
	}
	var lines []orders.Line
	for i, sp := range specs {
		off := uuid.NewV7()
		e.terms.mu.Lock()
		e.terms.triggers[off] = sp.trigger
		e.terms.mu.Unlock()
		l := orders.Line{ID: uuid.NewV7(), OrderID: o.ID, OrgID: o.OrgID, SellerOrgID: authztest.ID("club_a"), OfferingID: off,
			ServiceID: uuid.NewV7(), CommercePolicyID: uuid.NewV7(), TitleSnapshot: "Test stant paketi " + string(rune('A'+i)),
			Quantity: 1, UnitPriceAmount: 1_000_000, NetAmount: 1_000_000, VATRateBP: 2000, VATAmount: 200_000,
			VATWithheldAmount: sp.withheld, GrossAmount: 1_200_000, CommissionRateBP: 1000, CommissionBasis: "net_excl_vat",
			CommissionPayer: sp.payer, CommissionAmount: 100_000, CommissionVATRateBP: 2000, CommissionVATAmount: 20_000,
			SellerNetAmount: 1_080_000, PayableAmount: 1_200_000 - sp.withheld, FulfillmentStatus: orders.FulfillPending,
			CreatedAt: at, UpdatedAt: at}
		if sp.payee {
			py := e.payee
			l.PayeeID = &py
		} else {
			l.SellerOrgID, l.CommissionPayer, l.CommissionAmount, l.CommissionVATAmount = authztest.ID("company_a"), "seller", 0, 0
			l.FulfillmentStatus = orders.FulfillFulfilled
		}
		if sp.payer == "buyer" {
			l.BuyerFeeAmount, l.SellerNetAmount = 120_000, 1_000_000
		}
		if sp.fulfil {
			l.FulfillmentStatus = orders.FulfillFulfilled
		}
		lines = append(lines, l)
	}
	e.orders.mu.Lock()
	e.orders.orders[o.ID], e.orders.lines[o.ID] = o, lines
	e.orders.mu.Unlock()
	return o, lines
}

// deliver hands an event to billing's subscriptions of its topic.
func (e *env) deliver(t *testing.T, p domainevent.Payload) error {
	t.Helper()
	env, err := domainevent.New(p, "test", uuid.NewV7())
	if err != nil {
		t.Fatal(err)
	}
	return e.deliverEnv(t, env)
}

func (e *env) deliverEnv(t *testing.T, env domainevent.Envelope) error {
	t.Helper()
	subs := e.subs.For(env.Topic)
	if len(subs) == 0 {
		t.Fatalf("billing does not subscribe to %s", env.Topic)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var errs []error
	for _, s := range subs {
		errs = append(errs, s.HandleTask(t.Context(), raw))
	}
	return errors.Join(errs...)
}

// mustDeliver delivers and fails on an error.
func (e *env) mustDeliver(t *testing.T, p domainevent.Payload) {
	t.Helper()
	if err := e.deliver(t, p); err != nil {
		t.Fatalf("deliver %s: %v", p.Topic(), err)
	}
}

// invoices lists every invoice, oldest first.
func (e *env) invoices(t *testing.T, f billing.Filter) []billing.Invoice {
	t.Helper()
	rows, err := e.store.ListInvoices(t.Context(), f, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(rows)
	return rows
}

func (e *env) invoice(t *testing.T, id uuid.UUID) billing.Invoice {
	t.Helper()
	inv, err := e.store.GetInvoice(t.Context(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func (e *env) lines(t *testing.T, id uuid.UUID) []billing.Line {
	t.Helper()
	ls, err := e.store.InvoiceLines(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ls
}

// planned returns the service invoice planned for a paid model A order's
// first line (on_payment), i.e. a seller_uploaded invoice of club_a's payee.
func (e *env) planned(t *testing.T) billing.Invoice {
	t.Helper()
	o, _ := e.order(t, orders.ModelA, lineSpec{payee: true, trigger: catalog.InvoiceOnPayment, payer: "seller"})
	e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 1_200_000, OrderPaid: true})
	invs := e.invoices(t, billing.Filter{OrderID: &o.ID})
	if len(invs) != 1 || invs[0].Kind != billing.KindServiceSale {
		t.Fatalf("planned invoices = %+v", invs)
	}
	return invs[0]
}

// upload uploads a document to the invoice as name with a new PDF of name.
func (e *env) upload(t *testing.T, name string, id uuid.UUID, no string) (billing.InvoiceView, error) {
	t.Helper()
	a := actor(t, name)
	var org *uuid.UUID
	if inv := e.invoice(t, id); inv.IssuerOrgID != nil && name != "finance_admin" && name != "finance_admin_2" {
		org = inv.IssuerOrgID
	}
	file := e.files.put(name, org)
	return e.svc.UploadInvoice(ctxOf(t, a), a, id, billing.UploadInput{InvoiceNo: no, IssueDate: "2026-10-13",
		DocumentType: billing.DocEArsiv, PDFFileID: file})
}

func (e *env) mustUpload(t *testing.T, name string, id uuid.UUID, no string) billing.InvoiceView {
	t.Helper()
	v, err := e.upload(t, name, id, no)
	if err != nil {
		t.Fatalf("UploadInvoice(%s): %v", name, err)
	}
	return v
}

func (e *env) verify(t *testing.T, name string, id uuid.UUID, result billing.Status, note string) (billing.InvoiceView, error) {
	t.Helper()
	a := actor(t, name)
	return e.svc.AdminVerifyInvoice(ctxOf(t, a), a, id, billing.VerifyInput{Result: result, Note: note})
}

// issued returns an issued service invoice (uploaded by club_admin,
// verified by finance_admin).
func (e *env) issued(t *testing.T) billing.Invoice {
	t.Helper()
	inv := e.planned(t)
	e.mustUpload(t, "club_admin", inv.ID, "VKF2026000000123")
	v, err := e.verify(t, "finance_admin", inv.ID, billing.StatusIssued, "")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return v.Invoice
}

// runNotices runs the queued A notices.
func (e *env) runNotices(t *testing.T) {
	t.Helper()
	for _, task := range e.queue.Tasks() {
		if task.Type != billing.NotifyInvoiceTaskName {
			continue
		}
		if err := e.mod.Tasks()[0].Handle(t.Context(), task.Payload); err != nil {
			t.Fatalf("notice: %v", err)
		}
	}
	e.queue.Reset()
}
