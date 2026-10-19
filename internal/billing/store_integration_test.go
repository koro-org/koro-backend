//go:build integration

package billing_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgconn"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/billing"
	"koro/internal/catalog"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var seq atomic.Int64

func n() string { return strconv.FormatInt(seq.Add(1), 10) }

// pgWorld writes the parent rows of invoices through the app_rw pool
// (02 §17–19 and the BE-034 schema tests' pattern).
type pgWorld struct {
	t *testing.T
	d *db.DB
}

func (w *pgWorld) exec(sql string, args ...any) {
	w.t.Helper()
	if _, err := w.d.Pool.Exec(context.Background(), sql, args...); err != nil {
		w.t.Fatalf("%s: %v", sql, err)
	}
}

type stmt struct {
	sql  string
	args []any
}

// inTx runs the statements in one app_rw transaction (deferred checks:
// an order and its lines commit together).
func (w *pgWorld) inTx(stmts ...stmt) {
	w.t.Helper()
	ctx := context.Background()
	tx, err := w.d.Pool.Begin(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			w.t.Fatalf("%s: %v", s.sql, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		w.t.Fatalf("commit: %v", err)
	}
}

func (w *pgWorld) userWithID(id uuid.UUID) uuid.UUID {
	w.t.Helper()
	w.exec(`INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
		VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company') ON CONFLICT (id) DO NOTHING`, id, "u"+n()+"@example.test")
	return id
}

func (w *pgWorld) user() uuid.UUID { return w.userWithID(uuid.NewV7()) }

func (w *pgWorld) orgWithID(id uuid.UUID, kind string) uuid.UUID {
	w.t.Helper()
	k := n()
	w.exec(`INSERT INTO organizations (id, kind, name, slug) VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING`,
		id, kind, "Org "+k, "org-"+k)
	return id
}

func (w *pgWorld) org(kind string) uuid.UUID { return w.orgWithID(uuid.NewV7(), kind) }

func (w *pgWorld) party(kind, legalType string, org *uuid.UUID) uuid.UUID {
	w.t.Helper()
	id, k := uuid.NewV7(), n()
	w.exec(`INSERT INTO legal_parties (id, kind, legal_type, org_id, title, tax_no, email) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, kind, legalType, org, "Taraf "+k, strings.Repeat("0", 10-len(k))+k, "fatura"+k+"@example.test")
	return id
}

func (w *pgWorld) file() uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO files (id, purpose, bucket, storage_key, declared_mime) VALUES ($1, 'invoice_pdf', 'koro-files-dev', $2, 'application/pdf')`,
		id, "invoice_pdf/2026/10/"+id.String())
	return id
}

// shop is a seller club with its foundation payee and a company buyer.
type shop struct {
	club, company, payee, payeeParty, buyerParty, offering, service, policy uuid.UUID
}

func (w *pgWorld) shop(club, company uuid.UUID) shop {
	w.t.Helper()
	s := shop{club: club, company: company}
	uniOrg := w.org("university")
	uni := uuid.NewV7()
	w.exec(`INSERT INTO universities (id, org_id, yok_code, name, city, type) VALUES ($1, $2, $3, $4, 'Ankara', 'state')`,
		uni, uniOrg, "YOK"+n(), "Üniversite "+n())
	w.orgWithID(club, "club")
	w.exec(`INSERT INTO clubs (org_id, university_id, category) VALUES ($1, $2, 'academic') ON CONFLICT (org_id) DO NOTHING`, club, uni)
	w.orgWithID(company, "company")
	s.payeeParty = w.party("payee", "foundation", nil)
	s.buyerParty = w.party("buyer_company", "company", &company)
	s.payee = uuid.NewV7()
	w.exec(`INSERT INTO payees (id, legal_party_id, org_id, status, verified_at, verified_by, created_by)
		VALUES ($1, $2, $3, 'active', now(), $4, $5)`, s.payee, s.payeeParty, club, w.user(), w.user())
	s.service, s.offering, s.policy = uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	w.exec(`INSERT INTO services (id, code, name_tr, category, fulfillment_type) VALUES ($1, $2, 'Stant', 'booth', 'org_bound')`,
		s.service, "booth_"+n())
	w.exec(`INSERT INTO offerings (id, service_id, seller_kind, org_id, default_payee_id, title, unit_price_amount, vat_rate_bp,
		status, submitted_at, approved_by, approved_at, created_by)
		VALUES ($1, $2, 'org', $3, $4, 'Bahar şenliği stant', 1000000, 2000, 'active', now(), $5, now(), $6)`,
		s.offering, s.service, club, s.payee, w.user(), w.user())
	w.exec(`INSERT INTO commerce_policies (id, scope, scope_id, model_code, collection_method, commission_rate_bp, commission_basis,
		commission_payer, valid_from, created_by)
		VALUES ($1, 'offering', $2, 'A', 'bank_transfer', 1000, 'net_excl_vat', 'seller', now() - interval '1 day', $3)`,
		s.policy, s.offering, w.user())
	return s
}

// order writes a paid model A order of the shop with one line and returns
// the rows as orders sees them.
func (w *pgWorld) order(s shop) (orders.Order, orders.Line) {
	w.t.Helper()
	order, line, k := uuid.NewV7(), uuid.NewV7(), n()
	placer := w.user()
	w.inTx(
		stmt{`INSERT INTO orders (id, order_no, org_id, placed_by_user_id, buyer_party_id, buyer_snapshot, model_code, collection_method,
		subtotal_amount, vat_amount, total_amount, payable_amount, koro_collect_amount, expires_at)
		VALUES ($1, $2, $3, $4, $5, '{"title":"Örnek A.Ş."}'::jsonb, 'A', 'bank_transfer', 1000000, 200000, 1200000, 1200000, 1200000,
		now() + interval '7 days')`, []any{order, "KR" + strings.Repeat("0", 8-len(k)) + k, s.company, placer, s.buyerParty}},
		stmt{`INSERT INTO order_lines (id, order_id, org_id, seller_org_id, payee_id, offering_id, service_id, commerce_policy_id,
		title_snapshot, quantity, unit_price_amount, net_amount, vat_rate_bp, vat_amount, gross_amount, commission_rate_bp,
		commission_basis, commission_payer, commission_amount, commission_vat_rate_bp, commission_vat_amount, buyer_fee_amount,
		seller_net_amount, payable_amount, fulfillment_status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'Bahar şenliği stant', 1, 1000000, 1000000, 2000, 200000, 1200000, 1000,
		'net_excl_vat', 'seller', 100000, 2000, 20000, 0, 1080000, 1200000, 'pending')`,
			[]any{line, order, s.company, s.club, s.payee, s.offering, s.service, s.policy}},
	)
	w.exec(`UPDATE orders SET status = 'paid', paid_at = now(), paid_amount = koro_collect_amount WHERE id = $1`, order)
	payee := s.payee
	o := orders.Order{ID: order, OrgID: s.company, BuyerPartyID: s.buyerParty, BuyerSnapshot: orders.BuyerSnapshot{Title: "Örnek A.Ş."},
		ModelCode: orders.ModelA, CollectionMethod: orders.CollectBankTransfer, Status: orders.StatusPaid, Currency: "TRY"}
	l := orders.Line{ID: line, OrderID: order, OrgID: s.company, SellerOrgID: s.club, PayeeID: &payee, OfferingID: s.offering,
		ServiceID: s.service, CommercePolicyID: s.policy, TitleSnapshot: "Bahar şenliği stant", Quantity: 1, UnitPriceAmount: 1_000_000,
		NetAmount: 1_000_000, VATRateBP: 2000, VATAmount: 200_000, GrossAmount: 1_200_000, CommissionPayer: "seller",
		CommissionAmount: 100_000, CommissionVATRateBP: 2000, CommissionVATAmount: 20_000, SellerNetAmount: 1_080_000,
		PayableAmount: 1_200_000, FulfillmentStatus: orders.FulfillPending}
	return o, l
}

// worldOf is the contract's world over w.
func worldOf(w *pgWorld) world {
	var sh *shop
	return world{
		user:  func(*testing.T) uuid.UUID { return w.user() },
		party: func(*testing.T) uuid.UUID { return w.party("payee", "foundation", nil) },
		org:   func(_ *testing.T, kind string) uuid.UUID { return w.org(kind) },
		order: func(*testing.T) (uuid.UUID, uuid.UUID) {
			if sh == nil {
				s := w.shop(uuid.NewV7(), uuid.NewV7())
				sh = &s
			}
			o, l := w.order(*sh)
			return o.ID, l.ID
		},
		file: func(*testing.T) uuid.UUID { return w.file() },
		tx:   w.d.Tx,
	}
}

func pgStoreWorld(t *testing.T) (billing.Store, world) {
	d := testutil.NewDB(t)
	return billing.NewGormStore(d.ORM), worldOf(&pgWorld{t: t, d: d})
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, pgStoreWorld)
}

// constraintOf returns the violated constraint of err.
func constraintOf(err error) string {
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pe.ConstraintName
	}
	return ""
}

// TestVerify_UploaderCannotVerify_DBCheckPostgres: the database refuses a
// verification by the uploader even past the service and the store (06
// H-19: invoices_verifier_chk), and a verifier without an uploader; the
// store answers authz.four_eyes_violation.
func TestVerify_UploaderCannotVerify_DBCheckPostgres(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	pw := &pgWorld{t: t, d: d}
	w, s := worldOf(pw), billing.NewGormStore(d.ORM)
	ctx := t.Context()
	f := newFixture(t, w)
	inv, _ := insertIn(t, s, w, f)
	uploader := w.user(t)
	if _, err := updateIn(ctx, s, w, uploaded(inv, "VKF-DB-1", uploader, w.file(t)), billing.StatusPlanned); err != nil {
		t.Fatal(err)
	}
	_, err := d.Pool.Exec(ctx, `UPDATE invoices SET verified_by = uploaded_by, verified_at = now() WHERE id = $1`, inv.ID)
	if got := constraintOf(err); got != "invoices_verifier_chk" {
		t.Fatalf("the uploader verified past the store: %v (constraint %q)", err, got)
	}
	_, err = d.Pool.Exec(ctx, `UPDATE invoices SET status = 'issued', verified_by = uploaded_by, verified_at = now(),
		issued_at = now() WHERE id = $1`, inv.ID)
	if got := constraintOf(err); got != "invoices_verifier_chk" {
		t.Fatalf("issued by the uploader: %v (constraint %q)", err, got)
	}
	cur, err := s.GetInvoice(ctx, inv.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	cur.VerifiedBy, cur.VerifiedAt = cur.UploadedBy, &now
	if _, err := updateIn(ctx, s, w, cur, billing.StatusPlanned); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("store: %v, want authz.four_eyes_violation", err)
	}
	// A verifier needs an uploader (a NULL uploader is no pass).
	g := newFixture(t, w)
	other, _ := insertIn(t, s, w, g)
	_, err = d.Pool.Exec(ctx, `UPDATE invoices SET verified_by = $2, verified_at = now() WHERE id = $1`, other.ID, w.user(t))
	if got := constraintOf(err); got != "invoices_verifier_chk" {
		t.Fatalf("verified without an upload: %v (constraint %q)", err, got)
	}
}

// TestService_PlanUploadVerifyCancel_Postgres runs the manual flow over
// PostgreSQL: the plan of a paid order (twice: one invoice), the club's
// upload, the second eye's issue (invoice.issued), the cancellation with a
// credit note — the GORM mapping (jsonb snapshots, dates, bytea) and the
// CHECKs of 000116 on real rows.
func TestService_PlanUploadVerifyCancel_Postgres(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	pw := &pgWorld{t: t, d: d}
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	e := newEnvWith(t, clk, billing.NewGormStore(d.ORM), d.Tx)
	sh := pw.shop(authztest.ID("club_a"), authztest.ID("company_a"))
	koro := pw.party("platform", "company", nil)
	for _, p := range []string{"club_admin", "finance_admin", "finance_admin_2"} {
		pw.userWithID(authztest.ID(p))
	}
	e.parties.mu.Lock()
	e.parties.parties[koro] = parties.LegalParty{ID: koro, Kind: parties.KindPlatform, Title: "Koro"}
	e.parties.parties[sh.payeeParty] = parties.LegalParty{ID: sh.payeeParty, Kind: parties.KindPayee, Title: "Vakıf"}
	e.parties.payees[sh.payee] = parties.Payee{ID: sh.payee, LegalPartyID: sh.payeeParty, OrgID: sh.club, Status: parties.PayeeActive}
	e.parties.platform = &koro
	e.parties.mu.Unlock()
	o, l := pw.order(sh)
	e.terms.mu.Lock()
	e.terms.triggers[sh.offering] = catalog.InvoiceOnPayment
	e.terms.mu.Unlock()
	e.orders.mu.Lock()
	e.orders.orders[o.ID], e.orders.lines[o.ID] = o, []orders.Line{l}
	e.orders.mu.Unlock()

	for range 2 {
		e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 1_200_000, OrderPaid: true})
	}
	invs := e.invoices(t, billing.Filter{OrderID: &o.ID})
	if len(invs) != 1 || invs[0].IssuerPartyID != sh.payeeParty || invs[0].RecipientPartyID != sh.buyerParty ||
		invs[0].IssuerSnapshot.Title != "Vakıf" || invs[0].RecipientSnapshot.Title != "Örnek A.Ş." {
		t.Fatalf("planned = %+v", invs)
	}
	inv := invs[0]

	file := pw.file()
	by, club := authztest.ID("club_admin"), sh.club
	e.files.mu.Lock()
	e.files.meta[file] = filesMeta(file, by, &club)
	e.files.mu.Unlock()
	cla := actor(t, "club_admin")
	ettn := uuid.NewV7()
	v, err := e.svc.UploadInvoice(ctxOf(t, cla), cla, inv.ID, billing.UploadInput{InvoiceNo: "VKF2026000000777",
		IssueDate: "2026-10-01", DocumentType: billing.DocEFatura, PDFFileID: file, ETTN: &ettn})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if v.Invoice.IssueDate == nil || v.Invoice.IssueDate.String() != "2026-10-01" || v.Invoice.ETTN == nil || *v.Invoice.ETTN != ettn ||
		len(v.Invoice.PDFSHA256) != 32 {
		t.Fatalf("uploaded row = %+v", v.Invoice)
	}
	_, err = e.verify(t, "club_admin", inv.ID, billing.StatusIssued, "")
	if err == nil {
		t.Fatal("the club admin verified")
	}
	got, err := e.verify(t, "finance_admin_2", inv.ID, billing.StatusIssued, "")
	if err != nil || got.Invoice.Status != billing.StatusIssued || got.Invoice.IssuedAt == nil {
		t.Fatalf("verify = %+v, %v", got.Invoice, err)
	}
	fin := actor(t, "finance_admin")
	r, err := e.svc.AdminCancelInvoice(ctxOf(t, fin), fin, inv.ID, "Hizmet iptal edildi")
	if err != nil || r.CreditNote == nil || r.Invoice.Status != billing.StatusCancelled {
		t.Fatalf("cancel = %+v, %v", r, err)
	}
	if lines := e.lines(t, r.CreditNote.ID); len(lines) != 1 || lines[0].OrderLineID == nil || *lines[0].OrderLineID != l.ID {
		t.Fatalf("credit note lines = %+v", lines)
	}
}
