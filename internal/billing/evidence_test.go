package billing_test

import (
	"net/http"
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/billing"
	"koro/internal/files"
	"koro/internal/files/filestest"
	"koro/internal/notify"
	"koro/internal/platform/problem/problemtest"
)

// infected marks a PDF rejected by the scan after its upload.
func (e *env) infected(id uuid.UUID) {
	e.files.edit(id, func(m *files.FileMeta) { m.ScanStatus = files.ScanInfected })
}

// replacePDF replaces the invoice's PDF as name with a new PDF of name
// (of the issuer org for its admins).
func (e *env) replacePDF(t *testing.T, name string, id uuid.UUID) (billing.InvoiceView, error) {
	t.Helper()
	a := actor(t, name)
	var org *uuid.UUID
	if inv := e.invoice(t, id); inv.IssuerOrgID != nil && name == "club_admin" {
		org = inv.IssuerOrgID
	}
	return e.svc.ReplaceInvoicePDF(ctxOf(t, a), a, id, e.files.put(name, org))
}

// pdfNotified returns who got billing.invoice_pdf_rejected in-app.
func (e *env) pdfNotified() []uuid.UUID {
	var out []uuid.UUID
	for _, m := range e.sender.all() {
		if m.Template == "billing.invoice_pdf_rejected" && m.Channel == notify.ChannelInApp {
			out = append(out, *m.UserID)
		}
	}
	return out
}

// TestReplaceInvoicePDF_IssuedResetsVerification (BE-154b): an issued
// invoice's PDF that the scan rejects later shows rejected; the uploader
// side and finance are told; the issuer org's admin replaces it: the
// invoice stays issued with its number, the new PDF is unverified
// (pdf_replaced_at), only the uploader side and finance read it, and
// another person than its uploader verifies it again (result issued only,
// no second invoice.issued); a clean PDF stays.
func TestReplaceInvoicePDF_IssuedResetsVerification(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	inv := e.issued(t)
	old := *inv.PDFFileID
	_, err := e.replacePDF(t, "club_admin", inv.ID)
	problemtest.WantErr(t, err, files.ErrNotRejected)
	e.infected(old)
	cma := actor(t, "company_admin")
	v, err := e.svc.GetInvoice(ctxOf(t, cma), cma, inv.ID)
	if err != nil || files.StateOf(v.PDFStates, v.Invoice.PDFFileID) != files.StateRejected {
		t.Fatalf("view = %+v, %v; want the PDF rejected", v.PDFStates, err)
	}
	if err := filestest.Reject(t, e.mod.Subscribe, billing.NotifyRejectedPDFTaskName, old, files.PurposeInvoicePDF,
		files.RejectInfected, "billing", inv.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.pdfNotified(); !slices.Contains(got, authztest.ID("club_admin")) || !slices.Contains(got, authztest.ID("finance_admin")) ||
		slices.Contains(got, authztest.ID("company_admin")) {
		t.Fatalf("notice to %v, want the issuer's admins and finance", got)
	}
	_, err = e.replacePDF(t, "company_admin", inv.ID)
	problemtest.WantErr(t, err, authz.ErrForbidden)

	r, err := e.replacePDF(t, "club_admin", inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := r.Invoice
	if got.Status != billing.StatusIssued || *got.PDFFileID == old || got.UploadedBy == nil || *got.UploadedBy != authztest.ID("club_admin") ||
		got.VerifiedBy != nil || got.VerifiedAt != nil || got.PDFReplacedAt == nil || *got.InvoiceNo != *inv.InvoiceNo ||
		!got.IssuedAt.Equal(*inv.IssuedAt) || !got.AwaitsReverification() {
		t.Fatalf("replaced = %+v", got)
	}
	var found bool
	for _, ev := range e.audit.Of(authz.BillingInvoiceUpload) {
		if ev.Metadata["operation"] == "replace_rejected_file" {
			found = ev.Metadata["replaced_file_id"] == old.String() && ev.Metadata["verification_reset"] == true
		}
	}
	if !found {
		t.Fatal("no replace_rejected_file audit record with the reset verification")
	}
	pdf := func(name string) error {
		a := actor(t, name)
		_, err := e.svc.InvoicePDF(ctxOf(t, a), a, inv.ID)
		return err
	}
	problemtest.WantErr(t, pdf("company_admin"), billing.ErrInvalidState)
	if err := pdf("finance_admin"); err != nil {
		t.Fatalf("finance reads the replaced PDF: %v", err)
	}

	issued := len(e.outbox.Topics())
	_, err = e.verify(t, "finance_admin", inv.ID, billing.StatusMismatch, "Tutar tutmuyor")
	wantStatus(t, err, http.StatusUnprocessableEntity)
	_, err = e.verify(t, "club_admin", inv.ID, billing.StatusIssued, "")
	problemtest.WantErr(t, err, authz.ErrForbidden)
	rv, err := e.verify(t, "finance_admin_2", inv.ID, billing.StatusIssued, "")
	if err != nil {
		t.Fatal(err)
	}
	if rv.Invoice.Status != billing.StatusIssued || rv.Invoice.VerifiedBy == nil || *rv.Invoice.VerifiedBy != authztest.ID("finance_admin_2") ||
		rv.Invoice.AwaitsReverification() || len(e.outbox.Topics()) != issued {
		t.Fatalf("re-verified = %+v, %d events (want no new invoice.issued)", rv.Invoice, len(e.outbox.Topics())-issued)
	}
	if err := pdf("company_admin"); err != nil {
		t.Fatalf("the recipient reads the verified PDF: %v", err)
	}
	_, err = e.verify(t, "finance_admin", inv.ID, billing.StatusIssued, "")
	problemtest.WantErr(t, err, billing.ErrInvalidState)
	_, err = e.replacePDF(t, "club_admin", inv.ID)
	problemtest.WantErr(t, err, files.ErrNotRejected)
}

// TestReplaceInvoicePDF_BeforeIssue: an upload waiting for its
// verification gets a new PDF; the replacer is its uploader (and may not
// verify it); a mismatch is uploaded again with uploadInvoice (409 here);
// a frozen issued document refuses a store write that bypasses the rules.
func TestReplaceInvoicePDF_BeforeIssue(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	inv := e.planned(t)
	up := e.mustUpload(t, "club_admin", inv.ID, "VKF2026000000777")
	e.infected(*up.Invoice.PDFFileID)
	r, err := e.replacePDF(t, "finance_admin", inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Invoice; got.Status != billing.StatusPlanned || *got.UploadedBy != authztest.ID("finance_admin") || got.PDFReplacedAt != nil {
		t.Fatalf("replaced = %+v", got)
	}
	_, err = e.verify(t, "finance_admin", inv.ID, billing.StatusIssued, "")
	problemtest.WantErr(t, err, authz.ErrFourEyes)
	if _, err := e.verify(t, "finance_admin_2", inv.ID, billing.StatusIssued, ""); err != nil {
		t.Fatal(err)
	}

	cur := e.invoice(t, inv.ID)
	by := authztest.ID("platform_admin")
	cur.UploadedBy = &by
	if _, err := e.store.UpdateInvoice(t.Context(), cur, billing.StatusIssued); err == nil {
		t.Fatal("the store let the uploader of an issued invoice change without a new PDF")
	}

	other := e.planned(t)
	e.mustUpload(t, "club_admin", other.ID, "VKF2026000000778")
	if _, err := e.verify(t, "finance_admin", other.ID, billing.StatusMismatch, "Numara yanlış"); err != nil {
		t.Fatal(err)
	}
	_, err = e.replacePDF(t, "club_admin", other.ID)
	problemtest.WantErr(t, err, billing.ErrInvalidState)
}
