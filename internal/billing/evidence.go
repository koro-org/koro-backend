package billing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/notify"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// The rejected PDF of an uploaded invoice (T-393, T-397; BE-154b). A PDF
// that the scan rejects after its upload stays referenced by the invoice
// (a financial record, BE-151; the old file row stays attached as its
// history): the invoice shows the state of its PDF, finance and the
// uploader side are told, and the people who upload the invoice attach a
// new PDF in its place (billing.invoice.upload's cell, no new action):
//   - before the issue (an upload waiting for the verification) the new
//     PDF waits for the verification as the old one did;
//   - after the issue the invoice stays issued with its number, ETTN, date
//     and amounts (000164: invoices_frozen); the new PDF's checksum is
//     written, its verification is reset (pdf_replaced_at) and another
//     person than its uploader verifies it again (06 H-19,
//     adminVerifyInvoice); until then only the uploader side and finance
//     read it.

// pdfReplaceable reports an invoice whose PDF may be replaced: an uploaded
// invoice waiting for its verification, or issued.
func pdfReplaceable(inv Invoice) bool {
	return inv.IssuingMode.Uploaded() && inv.PDFFileID != nil && (inv.AwaitsVerification() || inv.Status == StatusIssued)
}

// ReplaceInvoicePDF implements Service: authz on the invoice (404 / 403;
// k29) → the new PDF (an active invoice_pdf PDF of the actor or of the
// issuer org; 422) → an uploaded invoice waiting for its verification or
// issued (409 billing.invalid_state) → in one transaction: the invoice
// locked and checked again, authz again → its PDF rejected after the
// upload (409 files.not_rejected) and the new one attached → the new PDF,
// its checksum and uploader; the verification reset (after the issue:
// pdf_replaced_at) → audit → finance (but the uploader) is asked to verify.
func (s *service) ReplaceInvoicePDF(ctx context.Context, a authz.Actor, id, fileID uuid.UUID) (InvoiceView, error) {
	inv, err := s.load(ctx, a, authz.BillingInvoiceUpload, id)
	if err != nil {
		return InvoiceView{}, err
	}
	if fileID == uuid.Nil() {
		return InvoiceView{}, problem.Field("body.pdf_file_id", "is required: the new invoice_pdf upload")
	}
	f, err := s.uploadable(ctx, a, inv, fileID)
	if err != nil {
		return InvoiceView{}, err
	}
	if !pdfReplaceable(inv) {
		return InvoiceView{}, ErrInvalidState
	}
	hash, err := idempotency.Hash(struct {
		Op            string
		Invoice, File uuid.UUID
	}{"replace_invoice_pdf", id, fileID})
	if err != nil {
		return InvoiceView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetInvoice(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if !pdfReplaceable(cur) {
			return uuid.Nil(), ErrInvalidState
		}
		if err := authz.Can(ctx, a, authz.BillingInvoiceUpload, invoiceResource(cur)); err != nil {
			return uuid.Nil(), err
		}
		owner := files.Owner{Module: "billing", EntityID: cur.ID, Purpose: files.PurposeInvoicePDF}
		meta, err := files.Replace(ctx, s.d.Files, a, *cur.PDFFileID, f.ID, owner)
		if err != nil {
			return uuid.Nil(), err
		}
		by, file := a.UserID, f.ID
		next := cur
		next.PDFFileID, next.PDFSHA256, next.UploadedBy, next.VerifiedBy, next.VerifiedAt = &file, nil, &by, nil, nil
		if len(f.SHA256) == 32 {
			next.PDFSHA256 = f.SHA256
		}
		if cur.WasIssued() {
			now := s.now()
			next.PDFReplacedAt = &now
		}
		stored, err := s.d.Store.UpdateInvoice(ctx, next, cur.Status)
		if err != nil {
			return uuid.Nil(), err
		}
		meta["from"], meta["verification_reset"] = string(cur.Status), cur.VerifiedBy != nil
		if err := s.d.Audit.Log(ctx, invoiceEvent(a, authz.BillingInvoiceUpload, stored, "", meta)); err != nil {
			return uuid.Nil(), err
		}
		s.notice(ctx, stored, noticeUploaded)
		return stored.ID, nil
	})
	if err != nil {
		return InvoiceView{}, translate(err)
	}
	return s.reload(ctx, id)
}

// ---- file.rejected

// NotifyRejectedPDFTaskName is the file.rejected subscription of the
// invoice PDFs (BE-154b, T-397).
const NotifyRejectedPDFTaskName = "billing.notify_rejected_pdf"

// tmplPDFRejected: in-app; finance and, for the payee's own invoice, the
// issuer org's admins (transactional, preference orders_payments).
const tmplPDFRejected = "billing.invoice_pdf_rejected"

// notifyRejectedPDF tells the people who upload an uploaded invoice's
// rejected PDF again (the invoice keeps the reference): every finance
// admin (the admin panel) and, for a seller_uploaded invoice, the issuer
// org's admins (their org's panel). An invoice that holds another PDF by
// now (replaced) or is cancelled is silent; the dedupe keys (the file and
// the recipient) make a repeated delivery one notice. The uploader hears
// from files as before.
func (s *service) notifyRejectedPDF(ctx context.Context, r files.Rejection) error {
	inv, err := s.d.Store.GetInvoice(ctx, r.EntityID, false)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case inv.PDFFileID == nil || *inv.PDFFileID != r.FileID || inv.Status == StatusCancelled || !inv.IssuingMode.Uploaded():
		return nil
	}
	if s.d.Sender == nil || s.d.Staff == nil {
		return errors.New("billing: the invoice notices are not wired")
	}
	fins, err := s.d.Staff.FinanceAdmins(ctx)
	if err != nil {
		return fmt.Errorf("billing: finance admins: %w", err)
	}
	key := r.FileID.String()
	errs := []error{s.notifyInApp(ctx, inv, tmplPDFRejected, key, fins, notify.AdminInvoiceLink(inv.ID))}
	if inv.IssuingMode == ModeSellerUploaded && inv.IssuerOrgID != nil {
		admins, err := s.orgAdmins(ctx, *inv.IssuerOrgID)
		if err != nil {
			return err
		}
		admins = slices.DeleteFunc(admins, func(id uuid.UUID) bool { return slices.Contains(fins, id) })
		errs = append(errs, s.notifyInApp(ctx, inv, tmplPDFRejected, key, admins, notify.OrgInvoiceLink(*inv.IssuerOrgID, inv.ID)))
	}
	return errors.Join(errs...)
}
