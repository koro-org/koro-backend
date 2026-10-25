package billing_test

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/billing"
	"koro/internal/catalog"
	"koro/internal/files"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// TestUpload_OnlyPlanned: a document is uploaded only to a planned invoice
// without one (or one found mismatched: the uploader side uploads again);
// an invoice waiting for verification, issued or cancelled is 409. k29:
// the issuer's club admin uploads its service invoice, another club 404, the
// buyer 403. The PDF must be an invoice_pdf PDF of the actor or the issuer
// org (422).
func TestUpload_OnlyPlanned(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	inv := e.planned(t)
	club := authztest.ID("club_a")

	_, err := e.upload(t, "other_club_admin", inv.ID, "X1")
	problemtest.WantErr(t, err, authz.ErrNotFound)
	_, err = e.upload(t, "company_admin", inv.ID, "X1")
	problemtest.WantErr(t, err, authz.ErrForbidden)

	cla := actor(t, "club_admin")
	in := func(file uuid.UUID) billing.UploadInput {
		return billing.UploadInput{InvoiceNo: "VKF2026000000001", IssueDate: "2026-10-13", DocumentType: billing.DocEArsiv, PDFFileID: file}
	}
	other := e.files.put("other_club_admin", nil)
	_, err = e.svc.UploadInvoice(ctxOf(t, cla), cla, inv.ID, in(other))
	wantStatus(t, err, http.StatusUnprocessableEntity)
	png := e.files.put("club_admin", &club)
	e.files.edit(png, func(m *files.FileMeta) { m.MIME = files.MIMEPNG })
	_, err = e.svc.UploadInvoice(ctxOf(t, cla), cla, inv.ID, in(png))
	wantStatus(t, err, http.StatusUnprocessableEntity)
	bad := in(e.files.put("club_admin", &club))
	bad.IssueDate = "2026-10-15" // tomorrow
	_, err = e.svc.UploadInvoice(ctxOf(t, cla), cla, inv.ID, bad)
	wantStatus(t, err, http.StatusUnprocessableEntity)

	v := e.mustUpload(t, "club_admin", inv.ID, "VKF2026000000001")
	got := v.Invoice
	if got.Status != billing.StatusPlanned || got.UploadedBy == nil || *got.UploadedBy != cla.UserID || got.PDFFileID == nil ||
		got.InvoiceNo == nil || *got.InvoiceNo != "VKF2026000000001" || got.IssueDate == nil || got.DocumentType != billing.DocEArsiv {
		t.Fatalf("uploaded invoice = %+v", got)
	}
	if owner := e.files.attached[*got.PDFFileID]; owner.Module != "billing" || owner.EntityID != inv.ID {
		t.Fatalf("PDF attached to %+v", owner)
	}
	if n := len(e.audit.Of(authz.BillingInvoiceUpload)); n != 1 {
		t.Fatalf("upload audit events = %d", n)
	}

	// Waiting for verification: no second upload (the verifier sees one document).
	_, err = e.upload(t, "club_admin", inv.ID, "VKF2026000000002")
	problemtest.WantErr(t, err, billing.ErrInvalidState)

	// Mismatch: the club uploads again; the verification starts over.
	if _, err := e.verify(t, "finance_admin", inv.ID, billing.StatusMismatch, "KDV tutarı farklı"); err != nil {
		t.Fatal(err)
	}
	if m := e.invoice(t, inv.ID); m.Status != billing.StatusMismatch || m.LastError == nil || *m.LastError != "KDV tutarı farklı" {
		t.Fatalf("mismatch = %+v", m)
	}
	v = e.mustUpload(t, "club_admin", inv.ID, "VKF2026000000002")
	if v.Invoice.Status != billing.StatusPlanned || v.Invoice.VerifiedBy != nil || v.Invoice.LastError != nil {
		t.Fatalf("re-upload after mismatch = %+v", v.Invoice)
	}

	// Issued: final.
	if _, err := e.verify(t, "finance_admin", inv.ID, billing.StatusIssued, ""); err != nil {
		t.Fatal(err)
	}
	_, err = e.upload(t, "finance_admin", inv.ID, "VKF2026000000003")
	problemtest.WantErr(t, err, billing.ErrInvalidState)

	// Cancelled: final; and the issuer's numbers are unique.
	other2 := e.planned(t)
	fin := actor(t, "finance_admin")
	if _, err := e.svc.AdminCancelInvoice(ctxOf(t, fin), fin, other2.ID, "plan hatalı"); err != nil {
		t.Fatal(err)
	}
	_, err = e.upload(t, "club_admin", other2.ID, "VKF2026000000004")
	problemtest.WantErr(t, err, billing.ErrInvalidState)
	third := e.planned(t)
	_, err = e.upload(t, "club_admin", third.ID, "VKF2026000000002")
	problemtest.WantErr(t, err, billing.ErrInvoiceNoTaken)
}

// TestVerify_UploaderCannotVerify_403AndDBCheck pins the four eyes of the
// verification (03 §6.1, 06 H-19): the finance admin who uploaded Koro's
// invoice cannot verify it (403 authz.four_eyes_violation) and the store
// refuses a verifier equal to the uploader (invoices_verifier_chk; the
// PostgreSQL CHECK itself: TestVerify_UploaderCannotVerify_DBCheckPostgres).
// Another finance admin verifies; a platform admin is no eye.
func TestVerify_UploaderCannotVerify_403AndDBCheck(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o, ls := e.order(t, orders.ModelA, lineSpec{payee: true, trigger: catalog.InvoiceOnPayment, payer: "seller", fulfil: true})
	payout := uuid.NewV7()
	e.orders.update(ls[0].ID, func(l *orders.Line) { l.PayoutID = &payout })
	e.mustDeliver(t, domainevent.PayoutPaid{PayoutID: payout, PayeeID: e.payee, Amount: 1_080_000})
	cs := e.invoices(t, billing.Filter{PayoutID: &payout})
	if len(cs) != 1 {
		t.Fatalf("commission invoices = %d (order %s)", len(cs), o.ID)
	}
	inv := cs[0]

	// club_admin may read Koro's commission invoice but not upload it (k29: seller_uploaded only).
	_, err := e.upload(t, "club_admin", inv.ID, "KRO2026000000001")
	problemtest.WantErr(t, err, authz.ErrForbidden)
	e.mustUpload(t, "finance_admin", inv.ID, "KRO2026000000001")

	_, err = e.verify(t, "finance_admin", inv.ID, billing.StatusIssued, "")
	problemtest.WantErr(t, err, authz.ErrFourEyes)
	wantStatus(t, err, http.StatusForbidden)
	_, err = e.verify(t, "platform_admin", inv.ID, billing.StatusIssued, "")
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.verify(t, "company_admin", inv.ID, billing.StatusIssued, "")
	problemtest.WantErr(t, err, authz.ErrForbidden)
	if got := e.invoice(t, inv.ID); got.Status != billing.StatusPlanned || got.VerifiedBy != nil {
		t.Fatalf("after the refused verifications: %+v", got)
	}

	// The store (the database's CHECK) refuses the uploader as the verifier too.
	cur := e.invoice(t, inv.ID)
	now := t0
	cur.Status, cur.VerifiedBy, cur.VerifiedAt, cur.IssuedAt = billing.StatusIssued, cur.UploadedBy, &now, &now
	_, err = e.store.UpdateInvoice(t.Context(), cur, billing.StatusPlanned)
	problemtest.WantErr(t, err, authz.ErrFourEyes)

	// A mismatch needs its note; the second eye issues it.
	_, err = e.verify(t, "finance_admin_2", inv.ID, billing.StatusMismatch, "")
	wantStatus(t, err, http.StatusUnprocessableEntity)
	v, err := e.verify(t, "finance_admin_2", inv.ID, billing.StatusIssued, "")
	if err != nil {
		t.Fatal(err)
	}
	got := v.Invoice
	if got.Status != billing.StatusIssued || got.VerifiedBy == nil || *got.VerifiedBy != authztest.ID("finance_admin_2") ||
		got.IssuedAt == nil || got.VerifiedAt == nil {
		t.Fatalf("verified invoice = %+v", got)
	}
	if topics := e.outbox.Topics(); len(topics) != 1 || topics[0] != domainevent.TopicInvoiceIssued {
		t.Fatalf("outbox topics = %v, want invoice.issued", topics)
	}
	_, err = e.verify(t, "finance_admin_2", inv.ID, billing.StatusIssued, "")
	problemtest.WantErr(t, err, billing.ErrInvalidState)
}

// TestVerify_InfectedOrPendingPDF_409: the issue waits for the virus scan.
func TestVerify_InfectedOrPendingPDF_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	inv := e.planned(t)
	v := e.mustUpload(t, "club_admin", inv.ID, "VKF2026000000010")
	e.files.edit(*v.Invoice.PDFFileID, func(m *files.FileMeta) { m.ScanStatus = files.ScanPending })
	_, err := e.verify(t, "finance_admin", inv.ID, billing.StatusIssued, "")
	problemtest.WantErr(t, err, files.ErrNotClean)
	if _, err := e.verify(t, "finance_admin", inv.ID, billing.StatusMismatch, "Belge okunamıyor"); err != nil {
		t.Fatalf("a mismatch does not need a clean file: %v", err)
	}
}

// TestCancelIssued_PlansCreditNote: cancelling an issued invoice cancels it
// and plans its credit note (same parties, scope, amounts and lines; the
// payee's upload mode); a cancelled invoice and an issued credit note are
// final; an unissued plan is cancelled without a credit note.
func TestCancelIssued_PlansCreditNote(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	inv := e.issued(t)
	fin := actor(t, "finance_admin")

	_, err := e.svc.AdminCancelInvoice(ctxOf(t, actor(t, "platform_admin")), actor(t, "platform_admin"), inv.ID, "iade")
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.AdminCancelInvoice(ctxOf(t, fin), fin, inv.ID, " ")
	wantStatus(t, err, http.StatusUnprocessableEntity)

	r, err := e.svc.AdminCancelInvoice(ctxOf(t, fin), fin, inv.ID, "Hizmet verilmedi")
	if err != nil {
		t.Fatal(err)
	}
	if r.Invoice.Status != billing.StatusCancelled || r.Invoice.IssuedAt == nil || r.CreditNote == nil {
		t.Fatalf("cancel result = %+v", r)
	}
	cn := *r.CreditNote
	if cn.Kind != billing.KindCreditNote || cn.Status != billing.StatusPlanned || cn.RefersToInvoiceID == nil ||
		*cn.RefersToInvoiceID != inv.ID || cn.IssuerPartyID != inv.IssuerPartyID || cn.RecipientPartyID != inv.RecipientPartyID ||
		cn.IssuingMode != billing.ModeSellerUploaded || cn.Trigger != billing.TriggerManual || cn.TotalAmount != inv.TotalAmount ||
		cn.PayableAmount != inv.PayableAmount || cn.OrderID == nil || *cn.OrderID != *inv.OrderID ||
		cn.IdempotencyKey != "credit_note:"+inv.OrderID.String()+":"+inv.ID.String()+":1" {
		t.Fatalf("credit note = %+v", cn)
	}
	orig, mirror := e.lines(t, inv.ID), e.lines(t, cn.ID)
	if len(mirror) != len(orig) || mirror[0].NetAmount != orig[0].NetAmount || *mirror[0].OrderLineID != *orig[0].OrderLineID {
		t.Fatalf("credit note lines %+v, original %+v", mirror, orig)
	}
	if ev := e.audit.Of(authz.BillingInvoiceCancel); len(ev) != 2 || ev[1].Reason != "Hizmet verilmedi" {
		t.Fatalf("cancel audit = %+v", ev)
	}

	_, err = e.svc.AdminCancelInvoice(ctxOf(t, fin), fin, inv.ID, "tekrar")
	problemtest.WantErr(t, err, billing.ErrInvalidState)

	// The club uploads the credit note; once issued it is final.
	e.mustUpload(t, "club_admin", cn.ID, "VKF2026000000099")
	if _, err := e.verify(t, "finance_admin_2", cn.ID, billing.StatusIssued, ""); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AdminCancelInvoice(ctxOf(t, fin), fin, cn.ID, "kredi notu iptali")
	problemtest.WantErr(t, err, billing.ErrInvalidState)

	plan := e.planned(t)
	r, err = e.svc.AdminCancelInvoice(ctxOf(t, fin), fin, plan.ID, "Plan hatalı")
	if err != nil || r.Invoice.Status != billing.StatusCancelled || r.CreditNote != nil {
		t.Fatalf("cancel of a plan = %+v, %v", r, err)
	}
}

// TestInvoiceIssued_EmailsPDF: invoice.issued e-mails the PDF to the
// recipient party's invoice address (its current one, not the order's
// snapshot) and tells the recipient org's admins in-app; a PDF not cleared
// by the scan yet fails the task (retried); a cancelled invoice is not sent.
func TestInvoiceIssued_EmailsPDF(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	inv := e.issued(t)
	events := e.outbox.Events()
	if len(events) != 1 || events[0].Topic != domainevent.TopicInvoiceIssued || events[0].AggregateID != inv.ID {
		t.Fatalf("outbox = %+v", events)
	}

	e.files.notClean = true
	if err := e.deliverEnv(t, events[0]); err == nil {
		t.Fatal("an uncleared PDF was e-mailed")
	}
	e.files.notClean = false
	if err := e.deliverEnv(t, events[0]); err != nil {
		t.Fatal(err)
	}
	var mails int
	inApp := map[string]bool{} // by dedupe key: notify drops a repeated key (the retried task resends)
	for _, m := range e.sender.all() {
		if m.Template != "billing.invoice_issued" {
			continue
		}
		switch m.Channel {
		case "email":
			mails++
			if m.ToEmail != "fatura@alici.example.test" || m.UserID != nil || len(m.Attachments) != 1 ||
				m.Attachments[0].ContentType != "application/pdf" || !bytes.Equal(m.Attachments[0].Data, pdfBytes) ||
				m.Attachments[0].Filename != "fatura-VKF2026000000123.pdf" ||
				m.DedupeKey != "billing.invoice_issued:"+inv.ID.String()+":email" {
				t.Fatalf("invoice e-mail = %+v", m)
			}
			if m.Data["invoice_no"] != "VKF2026000000123" || m.Data["issuer_title"] != "Test Vakfı" || m.Data["total_amount"] == "" {
				t.Fatalf("e-mail data = %v", m.Data)
			}
		case "in_app":
			inApp[m.DedupeKey] = true
			if m.UserID == nil || (*m.UserID != authztest.ID("company_admin") && *m.UserID != authztest.ID("company_owner_a")) ||
				m.LinkPath != "/invoices/"+inv.ID.String() {
				t.Fatalf("in-app notice = %+v", m)
			}
		}
	}
	if mails != 1 || len(inApp) != 2 {
		t.Fatalf("e-mails %d, in-app %d (company_a's admin and owner)", mails, len(inApp))
	}

	// Cancelled before the task ran: nothing is sent.
	fin := actor(t, "finance_admin")
	if _, err := e.svc.AdminCancelInvoice(ctxOf(t, fin), fin, inv.ID, "Yanlış kesildi"); err != nil {
		t.Fatal(err)
	}
	before := len(e.sender.all())
	if err := e.deliverEnv(t, events[0]); err != nil {
		t.Fatal(err)
	}
	if len(e.sender.all()) != before {
		t.Fatal("a cancelled invoice was sent")
	}
}

// TestNotices_UploadAndMismatch: an upload asks finance (but the uploader)
// to verify; a mismatch tells the uploader and the issuer org's admins.
func TestNotices_UploadAndMismatch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	inv := e.planned(t)
	e.runNotices(t)
	e.mustUpload(t, "club_admin", inv.ID, "VKF2026000000020")
	e.runNotices(t)
	if _, err := e.verify(t, "finance_admin", inv.ID, billing.StatusMismatch, "Tarih farklı"); err != nil {
		t.Fatal(err)
	}
	e.runNotices(t)
	got := map[string][]uuid.UUID{}
	for _, m := range e.sender.all() {
		got[m.Template] = append(got[m.Template], *m.UserID)
	}
	if u := got["billing.invoice_uploaded"]; len(u) != 2 {
		t.Fatalf("uploaded notices to %v, want both finance admins", u)
	}
	if u := got["billing.invoice_mismatch"]; len(u) != 1 || u[0] != authztest.ID("club_admin") {
		t.Fatalf("mismatch notices to %v, want club_admin", u)
	}
}

// TestInvoicePDF_Rules: an issued invoice's PDF for every reader; an
// unissued upload only for the uploader side and finance (the verifier).
func TestInvoicePDF_Rules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	inv := e.planned(t)
	pdf := func(name string) error {
		a := actor(t, name)
		_, err := e.svc.InvoicePDF(ctxOf(t, a), a, inv.ID)
		return err
	}
	problemtest.WantErr(t, pdf("finance_admin"), billing.ErrInvalidState) // nothing uploaded
	e.mustUpload(t, "club_admin", inv.ID, "VKF2026000000030")
	for name, want := range map[string]error{"club_admin": nil, "finance_admin": nil, "company_admin": billing.ErrInvalidState,
		"platform_admin": billing.ErrInvalidState, "other_club_admin": authz.ErrNotFound} {
		if err := pdf(name); want == nil && err != nil || want != nil && !errOf(err, want) {
			t.Fatalf("%s: %v, want %v", name, err, want)
		}
	}
	if _, err := e.verify(t, "finance_admin", inv.ID, billing.StatusIssued, ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"club_admin", "company_admin", "company_member", "platform_admin", "finance_admin"} {
		if err := pdf(name); err != nil {
			t.Fatalf("%s after the issue: %v", name, err)
		}
	}
}

func errOf(err, want error) bool {
	p, ok := errors.AsType[*problem.Error](err)
	w, wok := errors.AsType[*problem.Error](want)
	return ok && wok && p.Code() == w.Code()
}

// TestRefund_WholeLine: a refund of a whole order line cancels the line's
// unissued service invoice and plans a credit note of an issued one; a
// partial refund leaves the invoices to finance.
func TestRefund_WholeLine(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o, ls := e.order(t, orders.ModelA, lineSpec{payee: true, trigger: catalog.InvoiceOnPayment, payer: "seller"},
		lineSpec{payee: true, trigger: catalog.InvoiceOnPayment, payer: "seller"})
	e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 2_400_000, OrderPaid: true})
	invs := e.invoices(t, billing.Filter{OrderID: &o.ID})
	if len(invs) != 2 {
		t.Fatalf("planned %d", len(invs))
	}
	of := func(line uuid.UUID) billing.Invoice {
		for _, inv := range invs {
			if l := e.lines(t, inv.ID); *l[0].OrderLineID == line {
				return e.invoice(t, inv.ID)
			}
		}
		t.Fatalf("no invoice of line %s", line)
		return billing.Invoice{}
	}
	issuedOne := of(ls[1].ID)
	e.mustUpload(t, "club_admin", issuedOne.ID, "VKF2026000000040")
	if _, err := e.verify(t, "finance_admin", issuedOne.ID, billing.StatusIssued, ""); err != nil {
		t.Fatal(err)
	}

	// Partial refund: nothing changes.
	e.orders.update(ls[0].ID, func(l *orders.Line) { l.RefundedAmount = 100_000 })
	e.mustDeliver(t, domainevent.PaymentRefunded{RefundID: uuid.NewV7(), PaymentID: uuid.NewV7(), OrderID: o.ID,
		OrderLineID: ls[0].ID, Amount: 100_000})
	if got := of(ls[0].ID); got.Status != billing.StatusPlanned {
		t.Fatalf("after a partial refund: %s", got.Status)
	}
	// Whole lines: the plan is cancelled, the issued one gets a credit note.
	for _, l := range ls {
		e.orders.update(l.ID, func(l *orders.Line) { l.RefundedAmount = l.PayableAmount })
		for range 2 { // a repeated event changes nothing more
			e.mustDeliver(t, domainevent.PaymentRefunded{RefundID: uuid.NewV7(), PaymentID: uuid.NewV7(), OrderID: o.ID,
				OrderLineID: l.ID, Amount: 1_200_000})
		}
	}
	if got := of(ls[0].ID); got.Status != billing.StatusCancelled {
		t.Fatalf("refunded plan: %s", got.Status)
	}
	if got := of(ls[1].ID); got.Status != billing.StatusCancelled {
		t.Fatalf("refunded issued invoice: %s", got.Status)
	}
	notes := e.invoices(t, billing.Filter{Kind: billing.KindCreditNote})
	if len(notes) != 1 || *notes[0].RefersToInvoiceID != issuedOne.ID {
		t.Fatalf("credit notes = %+v", notes)
	}
	// An order-level refund (no line) changes no invoice.
	e.mustDeliver(t, domainevent.PaymentRefunded{RefundID: uuid.NewV7(), PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 5})
}

// TestListAndGet_OrgScope: the org surface lists the org's issued and
// received invoices (direction), the admin list filters, getInvoice is
// 404 for an outsider.
func TestListAndGet_OrgScope(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	inv := e.planned(t)
	list := func(name, org string, d billing.Direction) (billing.InvoicePage, error) {
		a := actor(t, name)
		return e.svc.ListOrgInvoices(ctxOf(t, a), a, authztest.ID(org), billing.ListQuery{Direction: d})
	}
	for _, c := range []struct {
		name, org string
		d         billing.Direction
		n         int
	}{
		{"club_admin", "club_a", "", 1}, {"club_admin", "club_a", billing.DirectionIssued, 1},
		{"club_admin", "club_a", billing.DirectionReceived, 0}, {"company_admin", "company_a", billing.DirectionReceived, 1},
		{"company_member", "company_a", "", 1}, {"finance_admin", "club_b", "", 0},
	} {
		p, err := list(c.name, c.org, c.d)
		if err != nil || len(p.Items) != c.n {
			t.Fatalf("%s %s %q: %d, %v; want %d", c.name, c.org, c.d, len(p.Items), err, c.n)
		}
	}
	_, err := list("club_member", "club_a", "")
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = list("other_club_admin", "club_a", "")
	problemtest.WantErr(t, err, authz.ErrNotFound)
	_, err = list("finance_admin", "no_such_org", "")
	problemtest.WantErr(t, err, problem.ErrNotFound)

	a := actor(t, "other_company_member")
	_, err = e.svc.GetInvoice(ctxOf(t, a), a, inv.ID)
	problemtest.WantErr(t, err, authz.ErrNotFound)
	a = actor(t, "company_admin")
	v, err := e.svc.GetInvoice(ctxOf(t, a), a, inv.ID)
	if err != nil || len(v.Lines) != 1 {
		t.Fatalf("GetInvoice = %+v, %v", v, err)
	}
	fin := actor(t, "finance_admin")
	p, err := e.svc.AdminListInvoices(ctxOf(t, fin), fin, billing.ListQuery{Status: billing.StatusPlanned, OrderID: inv.OrderID})
	if err != nil || len(p.Items) != 1 {
		t.Fatalf("admin list = %d, %v", len(p.Items), err)
	}
	_, err = e.svc.AdminListInvoices(ctxOf(t, actor(t, "support")), actor(t, "support"), billing.ListQuery{})
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.AdminListInvoices(ctxOf(t, fin), fin, billing.ListQuery{Status: "nope"})
	wantStatus(t, err, http.StatusUnprocessableEntity)
}

// TestNoticeTemplates_RenderWithSentData: the notices billing sends render
// with the data it sends (notify refuses a missing key) in both locales.
func TestNoticeTemplates_RenderWithSentData(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	inv := e.planned(t)
	e.runNotices(t)
	e.mustUpload(t, "club_admin", inv.ID, "VKF2026000000123")
	e.runNotices(t)
	if _, err := e.verify(t, "finance_admin", inv.ID, billing.StatusIssued, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.deliverEnv(t, e.outbox.Events()[0]); err != nil {
		t.Fatal(err)
	}
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range e.sender.all() {
		if m.Channel != notify.ChannelEmail {
			if _, ok := tpl.Notice(m.Template, "tr"); !ok {
				t.Fatalf("no in-app text of %s", m.Template)
			}
			seen[m.Template] = true
			continue
		}
		for _, loc := range []string{"tr", "en"} {
			r, err := tpl.Render(m.Template, loc, notify.RenderInput{Data: m.Data})
			if err != nil {
				t.Fatalf("render %s/%s: %v", m.Template, loc, err)
			}
			if !strings.Contains(r.Text, "VKF2026000000123") {
				t.Fatalf("%s/%s text misses the number: %s", m.Template, loc, r.Text)
			}
		}
		seen[m.Template] = true
	}
	for _, id := range []string{"billing.invoice_issued", "billing.invoice_planned", "billing.invoice_uploaded"} {
		if !seen[id] {
			t.Errorf("%s was not sent (invoice %s)", id, inv.ID)
		}
	}
	if _, ok := tpl.Notice("billing.invoice_mismatch", "en"); !ok {
		t.Error("no in-app text of billing.invoice_mismatch")
	}
}
