package ledger_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/ledger"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
)

// reauthPassword is the proof the fake re-authenticator accepts.
const reauthPassword = "correct-horse-battery"

// Valid TR IBANs (the matrix fixtures' numbers).
var testIBANs = []string{"TR330006100519786457841326", "TR950001200945200058000003", "TR809999900000000000000001",
	"TR039999901234567890123456"}

// fakeOrders is the orders port over in-memory lines: a line is payable
// while it is unpaid-out and fulfilled at or before asOf (hold 0).
type fakeOrders struct {
	mu        sync.Mutex
	lines     map[uuid.UUID]*fakeLine
	orders    map[uuid.UUID]string   // order → order_no
	marks     int                    // MarkLinesPaidOut calls that wrote
	collected []orders.CollectedLine // CollectedLines (BE-067), set by collect
}

type fakeLine struct {
	id, order, payee, seller uuid.UUID
	net                      int64
	fulfilled                time.Time
	paidOut                  *uuid.UUID
	refunded                 bool
}

func newFakeOrders() *fakeOrders {
	return &fakeOrders{lines: map[uuid.UUID]*fakeLine{}, orders: map[uuid.UUID]string{}}
}

func (f *fakeOrders) add(l fakeLine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines[l.id] = &l
	if _, ok := f.orders[l.order]; !ok {
		f.orders[l.order] = "KR" + l.order.String()[:8]
	}
}

func (f *fakeOrders) PayableLines(_ context.Context, payeeID uuid.UUID, asOf time.Time) ([]orders.PayableLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []orders.PayableLine
	for _, l := range f.lines {
		if l.payee != payeeID || l.paidOut != nil || l.refunded || l.fulfilled.After(asOf) {
			continue
		}
		out = append(out, orders.PayableLine{LineID: l.id, OrderID: l.order, OrderNo: f.orders[l.order], PayeeID: l.payee,
			SellerOrgID: l.seller, SellerNetAmount: l.net, FulfilledAt: l.fulfilled})
	}
	slices.SortFunc(out, func(a, b orders.PayableLine) int { return a.FulfilledAt.Compare(b.FulfilledAt) })
	return out, nil
}

func (f *fakeOrders) MarkLinesPaidOut(_ context.Context, lineIDs []uuid.UUID, payoutID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range lineIDs {
		if l, ok := f.lines[id]; !ok || l.paidOut != nil {
			return orders.ErrInvalidState
		}
	}
	for _, id := range lineIDs {
		p := payoutID
		f.lines[id].paidOut = &p
	}
	f.marks++
	return nil
}

func (f *fakeOrders) GetLine(_ context.Context, lineID uuid.UUID) (orders.Line, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.lines[lineID]
	if !ok {
		return orders.Line{}, orders.ErrNotFound
	}
	payee := l.payee
	return orders.Line{ID: l.id, OrderID: l.order, SellerOrgID: l.seller, PayeeID: &payee, SellerNetAmount: l.net,
		PayoutID: l.paidOut}, nil
}

func (f *fakeOrders) ForPayment(_ context.Context, orderID uuid.UUID) (orders.PayableOrder, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	no, ok := f.orders[orderID]
	if !ok {
		return orders.PayableOrder{}, orders.ErrNotFound
	}
	return orders.PayableOrder{ID: orderID, OrderNo: no}, nil
}

// collect adds lines of orders Koro collected in full (CollectedLines).
func (f *fakeOrders) collect(ls ...orders.CollectedLine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collected = append(f.collected, ls...)
	slices.SortFunc(f.collected, func(a, b orders.CollectedLine) int { return bytes.Compare(a.LineID[:], b.LineID[:]) })
}

// refund records a refunded amount on a settled line.
func (f *fakeOrders) refund(lineID uuid.UUID, amount int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.collected {
		if f.collected[i].LineID == lineID {
			f.collected[i].RefundedAmount += amount
		}
	}
}

func (f *fakeOrders) CollectedLines(_ context.Context, after uuid.UUID, platformOnly bool, limit int) ([]orders.CollectedLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []orders.CollectedLine{}
	for _, l := range f.collected {
		if bytes.Compare(l.LineID[:], after[:]) > 0 && (!platformOnly || l.PayeeID == nil) && len(out) < limit {
			out = append(out, l)
		}
	}
	return out, nil
}

func (f *fakeOrders) ForAllocation(_ context.Context, lineID uuid.UUID) (orders.CollectedLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.collected {
		if l.LineID == lineID {
			return l, nil
		}
	}
	return orders.CollectedLine{}, orders.ErrNotFound
}

func (f *fakeOrders) paidOut(id uuid.UUID) *uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lines[id].paidOut
}

// fakePayees holds payees, their approved accounts and parties.
type fakePayees struct {
	mu       sync.Mutex
	payees   map[uuid.UUID]parties.Payee
	accounts map[uuid.UUID]parties.BankAccount // approved, by payee
	parties  map[uuid.UUID]parties.LegalParty
}

func (f *fakePayees) Get(_ context.Context, id uuid.UUID) (parties.Payee, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.payees[id]
	if !ok {
		return parties.Payee{}, parties.ErrNotFound
	}
	return p, nil
}

func (f *fakePayees) ApprovedAccount(_ context.Context, id uuid.UUID) (parties.BankAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.accounts[id]
	if !ok {
		return parties.BankAccount{}, parties.ErrNoApprovedAccount
	}
	return b, nil
}

func (f *fakePayees) IsActive(_ context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.payees[id]
	if !ok {
		return false, parties.ErrNotFound
	}
	return p.Status == parties.PayeeActive, nil
}

// approve replaces the payee's approved account (the old one is superseded).
func (f *fakePayees) approve(payee uuid.UUID, ibanNo, holder string) parties.BankAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := parties.BankAccount{ID: uuid.NewV7(), PayeeID: payee, OrgID: f.payees[payee].OrgID, IBAN: ibanNo, HolderName: holder,
		Status: parties.AccountApproved}
	f.accounts[payee] = b
	return b
}

// fakeParties reads the parties of fakePayees.
type fakeParties struct{ p *fakePayees }

func (f fakeParties) Get(_ context.Context, id uuid.UUID) (parties.LegalParty, error) {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	lp, ok := f.p.parties[id]
	if !ok {
		return parties.LegalParty{}, parties.ErrNotFound
	}
	return lp, nil
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

// fakeFiles holds file metadata; Attach records the owner.
type fakeFiles struct {
	mu       sync.Mutex
	meta     map[uuid.UUID]files.FileMeta
	attached map[uuid.UUID]files.Owner
}

func (f *fakeFiles) put(purpose files.Purpose) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewV7()
	f.meta[id] = files.FileMeta{ID: id, Purpose: purpose, Status: files.StatusActive, ScanStatus: files.ScanSkipped,
		AttachedEntityID: uuid.Nil()}
	return id
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

func (f *fakeFiles) States(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]files.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID]files.State{}
	for _, id := range ids {
		if m, ok := f.meta[id]; ok {
			out[id] = m.State(false)
		}
	}
	return out, nil
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
	f.mu.Lock()
	m := f.meta[id]
	f.mu.Unlock()
	switch { // files' servable rule
	case m.Status == files.StatusRejected:
		return files.Download{}, files.ErrRejected
	case m.ScanStatus == files.ScanInfected:
		return files.Download{}, files.ErrNotClean
	}
	return files.Download{URL: "https://files.test/" + id.String(), ExpiresAt: t0.Add(ttl)}, nil
}

// reject takes a file out of use as files' scan does (file.rejected):
// infected or, when the metadata could not be removed, rejected.
func (f *fakeFiles) reject(id uuid.UUID, reason files.RejectReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.meta[id]
	if reason == files.RejectInfected {
		m.ScanStatus = files.ScanInfected
	} else {
		m.Status = files.StatusRejected
	}
	f.meta[id] = m
}

var errAuditDown = errors.New("audit is down")

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

// fakeStaff lists finance_admin and finance_admin_2, platform_admin and
// platform_admin_2.
type fakeStaff struct{}

func (fakeStaff) FinanceAdmins(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{authztest.ID("finance_admin"), authztest.ID("finance_admin_2")}, nil
}

func (fakeStaff) PlatformAdmins(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{authztest.ID("platform_admin"), authztest.ID("platform_admin_2")}, nil
}

// fakeUsers is identity's contact lookup over a fixed directory: the finance
// admins and platform_admin have an address; any other user (platform_admin_2
// among them) is identity.ErrNotFound, i.e. in-app only and named by ID.
type fakeUsers struct{}

// staffContacts is the directory of fakeUsers.
var staffContacts = map[string]identity.UserContact{
	"finance_admin":   {Email: "fin1@example.test", FirstName: "Fin", LastName: "Bir", Locale: "tr"},
	"finance_admin_2": {Email: "fin2@example.test", FirstName: "Fin", LastName: "İki", Locale: "en"},
	"platform_admin":  {Email: "pad1@example.test", FirstName: "Pad", LastName: "Bir", Locale: "tr"},
}

func (fakeUsers) Contact(_ context.Context, id uuid.UUID) (identity.UserContact, error) {
	for name, c := range staffContacts {
		if authztest.ID(name) == id {
			c.ID = id
			return c, nil
		}
	}
	return identity.UserContact{}, identity.ErrNotFound
}

// payoutEnv is the ledger with the payout flow over in-memory stores (or
// the given store, payout store and transactions) and the fakes above.
type payoutEnv struct {
	*env
	orders *fakeOrders
	payees *fakePayees
	files  *fakeFiles
	audit  *audittest.Recorder
	outbox *outbox.Mem
	sender *fakeSender
	reauth *audittest.LateReauth[identity.ReauthProof] // fakeReauth with an overload switch
}

func newPayoutEnv(t *testing.T) *payoutEnv {
	t.Helper()
	clk := clock.NewFake(t0)
	mem := ledger.NewMemStore(clk.Now)
	return newPayoutEnvWith(t, clk, mem, mem, db.NopTx{}, nil)
}

// newPayoutEnvWith builds the env; ord replaces the fake orders port when
// set (the PostgreSQL tests use the real orders module).
func newPayoutEnvWith(t *testing.T, clk *clock.Fake, store ledger.Store, payouts ledger.PayoutStore, tx db.TxManager,
	ord ledger.Orders) *payoutEnv {
	t.Helper()
	e := &payoutEnv{
		env:    &env{clk: clk, store: store, alarms: &alarms{}},
		orders: newFakeOrders(),
		payees: &fakePayees{payees: map[uuid.UUID]parties.Payee{}, accounts: map[uuid.UUID]parties.BankAccount{},
			parties: map[uuid.UUID]parties.LegalParty{}},
		files:  &fakeFiles{meta: map[uuid.UUID]files.FileMeta{}, attached: map[uuid.UUID]files.Owner{}},
		audit:  &audittest.Recorder{},
		outbox: outbox.NewMem(clk),
		sender: &fakeSender{},
		reauth: &audittest.LateReauth[identity.ReauthProof]{Next: fakeReauth{}},
	}
	var orderPort ledger.Orders = e.orders
	if ord != nil {
		orderPort = ord
	}
	keeper := idempotency.New(idempotency.Deps{Tx: tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()})
	mod, err := ledger.New(ledger.Deps{
		Store: store, Tx: tx, Clock: clk, Log: obs.Discard(), Alarm: e.alarms.raise,
		Payouts: payouts, Orders: orderPort, Payees: e.payees, Parties: fakeParties{p: e.payees}, Orgs: fakeOrgs{},
		Files: e.files, Audit: e.audit, Outbox: e.outbox, Idempotency: keeper, Reauth: e.reauth,
		Sender: e.sender, Staff: fakeStaff{}, Users: fakeUsers{},
	})
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
}

// seller registers an active payee of org with an approved account and a
// party with a contact address, and returns the payee.
func (e *payoutEnv) seller(t *testing.T, org string) uuid.UUID {
	t.Helper()
	orgID := authztest.ID(org)
	id, party := uuid.NewV7(), uuid.NewV7()
	mail := "vakif@example.test"
	e.payees.mu.Lock()
	e.payees.payees[id] = parties.Payee{ID: id, LegalPartyID: party, OrgID: orgID, Status: parties.PayeeActive}
	e.payees.parties[party] = parties.LegalParty{ID: party, Title: "Örnek Vakfı", Email: &mail}
	n := len(e.payees.accounts)
	e.payees.mu.Unlock()
	e.payees.approve(id, testIBANs[n%len(testIBANs)], "Örnek Vakfı")
	return id
}

// payable adds a fulfilled line of payee (seller net 1,080,000) and posts
// its payment and allocation, so the ledger owes the payee its share.
func (e *payoutEnv) payable(t *testing.T, payee uuid.UUID) uuid.UUID {
	t.Helper()
	l := line(payee)
	order := e.paidOrder(t, l)
	py, err := e.payees.Get(t.Context(), payee)
	if err != nil {
		t.Fatal(err)
	}
	e.orders.add(fakeLine{id: l.OrderLineID, order: order, payee: payee, seller: py.OrgID, net: l.SellerNetAmount,
		fulfilled: t0.Add(-24 * time.Hour)})
	return l.OrderLineID
}

// key returns ctx with an Idempotency-Key of a's POST /admin/payouts.
func key(ctx context.Context, a authz.Actor, value string) context.Context {
	return idempotency.WithKey(ctx, idempotency.Key{Scope: a.UserID.String() + ":POST /admin/payouts", Value: value, Status: 201})
}

// ctxOf is a request context of the actor (ledger rows record created_by).
func ctxOf(t *testing.T, a authz.Actor) context.Context { return authz.WithActor(t.Context(), a) }

// create creates a payout of the payee's payable lines by name.
func (e *payoutEnv) create(t *testing.T, name string, payee uuid.UUID, lines ...uuid.UUID) ledger.PayoutView {
	t.Helper()
	a := actor(t, name)
	v, err := e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee, LineIDs: lines})
	if err != nil {
		t.Fatalf("AdminCreatePayout: %v", err)
	}
	return v
}

// approve approves a payout by name with the right proof.
func (e *payoutEnv) approve(t *testing.T, name string, id uuid.UUID) ledger.PayoutView {
	t.Helper()
	a := actor(t, name)
	v, err := e.svc.AdminApprovePayout(ctxOf(t, a), a, id, identity.ReauthProof{Password: reauthPassword})
	if err != nil {
		t.Fatalf("AdminApprovePayout: %v", err)
	}
	return v
}

// markPaid marks an approved payout paid with a new receipt.
func (e *payoutEnv) markPaid(t *testing.T, name string, id uuid.UUID) ledger.PayoutView {
	t.Helper()
	a := actor(t, name)
	receipt := e.files.put(files.PurposePayoutReceipt)
	v, err := e.svc.AdminMarkPayoutPaid(ctxOf(t, a), a, id, ledger.MarkPaidInput{BankReference: "EFT-1", ReceiptFileID: &receipt})
	if err != nil {
		t.Fatalf("AdminMarkPayoutPaid: %v", err)
	}
	return v
}
