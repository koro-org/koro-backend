package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/orders"
	"koro/internal/platform/civil"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// Attrs of the invoice resource (03 §3.5: OrgID = issuer_org_id, Attrs
// recipient_org_id; BE-042 adds issuing_mode for 03 k29 and uploaded_by for
// the four eyes of the verification, 06 H-27).
const (
	attrRecipientOrg = "recipient_org_id"
	attrIssuingMode  = "issuing_mode"
)

// Limits of the invoice operations.
const (
	maxNoteRunes   = 500 // verify note, cancel reason
	pdfURLTTL      = 5 * time.Minute
	maxPDFBytes    = files.MaxUploadBytes // the e-mail attachment (invoice_pdf limit)
	invoiceSubject = "invoice"            // the aggregate type of invoice events
)

// invoiceNoRe is the number an upload may declare: e-Fatura/e-Arşiv numbers
// (3 letters, year, 9 digits) and paper series alike, at most 30 characters
// (invoices_invoice_no_chk).
var invoiceNoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/._-]{0,29}$`)

// adminSurface is the authz view of /admin/** (403 before reading; 03 §7.2-3).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// ListQuery narrows the invoice lists (listOrgInvoices, adminListInvoices).
type ListQuery struct {
	Direction Direction
	Kind      Kind
	Status    Status
	OrderID   *uuid.UUID
	PayoutID  *uuid.UUID
	Limit     int
	After     *Cursor
	invalid   error // a malformed query parameter or cursor (422 after authz)
}

// InvoicePage is a page of invoices.
type InvoicePage struct {
	Items []Invoice
	Next  *Cursor
}

// InvoiceView is an invoice with its lines.
type InvoiceView struct {
	Invoice Invoice
	Lines   []Line
}

// UploadInput is the body of uploadInvoice (04 §2.18).
type UploadInput struct {
	InvoiceNo    string
	IssueDate    string // YYYY-MM-DD
	DocumentType DocumentType
	PDFFileID    uuid.UUID
	ETTN         *uuid.UUID
}

// VerifyInput is the body of adminVerifyInvoice.
type VerifyInput struct {
	Result Status // issued | mismatch
	Note   string // required for mismatch
}

// CancelResult is the cancelled invoice and, for an issued one, the credit
// note planned for it.
type CancelResult struct {
	Invoice    Invoice
	CreditNote *Invoice
}

// now is the service clock in the database's precision.
func (s *service) now() time.Time { return stamp(s.d.Clock.Now()) }

func idStr(p *uuid.UUID) string {
	if p == nil {
		return ""
	}
	return p.String()
}

// invoiceResource is the authz view of one invoice.
func invoiceResource(inv Invoice) authz.Resource {
	return authz.Resource{Type: authz.TypeInvoice, ID: inv.ID, OrgID: inv.IssuerOrgID, Attrs: map[string]string{
		attrRecipientOrg: idStr(inv.RecipientOrgID), attrIssuingMode: string(inv.IssuingMode),
		authz.AttrUploadedBy: idStr(inv.UploadedBy),
	}}
}

// orgInvoices is the authz view of an org's invoices as a collection: the
// org issues and receives them (authorised before anything is read).
func orgInvoices(orgID uuid.UUID) authz.Resource {
	org := orgID
	return authz.Resource{Type: authz.TypeInvoice, OrgID: &org, Attrs: map[string]string{
		attrRecipientOrg: org.String(), attrIssuingMode: "", authz.AttrUploadedBy: "",
	}}
}

func fieldErr(location, msg string) error {
	return problem.ErrValidation.WithFields(problem.FieldError{Location: location, Message: msg})
}

// load reads an invoice and authorises act on it (404 when it does not
// exist or is not visible to a).
func (s *service) load(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID) (Invoice, error) {
	inv, err := s.d.Store.GetInvoice(ctx, id, false)
	if err != nil {
		return Invoice{}, err
	}
	if err := authz.Can(ctx, a, act, invoiceResource(inv)); err != nil {
		return Invoice{}, err
	}
	return inv, nil
}

func (s *service) view(ctx context.Context, inv Invoice) (InvoiceView, error) {
	lines, err := s.d.Store.InvoiceLines(ctx, inv.ID)
	if err != nil {
		return InvoiceView{}, err
	}
	return InvoiceView{Invoice: inv, Lines: lines}, nil
}

func (s *service) reload(ctx context.Context, id uuid.UUID) (InvoiceView, error) {
	inv, err := s.d.Store.GetInvoice(ctx, id, false)
	if err != nil {
		return InvoiceView{}, err
	}
	return s.view(ctx, inv)
}

// ---- reads

// ListOrgInvoices implements Service.
func (s *service) ListOrgInvoices(ctx context.Context, a authz.Actor, orgID uuid.UUID, q ListQuery) (InvoicePage, error) {
	if _, err := s.d.Orgs.Get(ctx, orgID); err != nil {
		return InvoicePage{}, err
	}
	if err := authz.Can(ctx, a, authz.BillingInvoiceList, orgInvoices(orgID)); err != nil {
		return InvoicePage{}, err
	}
	org := orgID
	return s.list(ctx, Filter{OrgID: &org, Direction: q.Direction}, q)
}

// AdminListInvoices implements Service.
func (s *service) AdminListInvoices(ctx context.Context, a authz.Actor, q ListQuery) (InvoicePage, error) {
	if err := authz.Can(ctx, a, authz.BillingInvoiceListAdmin, adminSurface); err != nil {
		return InvoicePage{}, err
	}
	return s.list(ctx, Filter{OrderID: q.OrderID, PayoutID: q.PayoutID}, q)
}

func (s *service) list(ctx context.Context, f Filter, q ListQuery) (InvoicePage, error) {
	switch {
	case q.invalid != nil:
		return InvoicePage{}, q.invalid
	case q.Kind != "" && !q.Kind.Valid():
		return InvoicePage{}, fieldErr("query.kind", "is not an invoice kind")
	case q.Status != "" && !q.Status.Valid():
		return InvoicePage{}, fieldErr("query.status", "is not an invoice status")
	case q.Direction != "" && q.Direction != DirectionIssued && q.Direction != DirectionReceived:
		return InvoicePage{}, fieldErr("query.direction", "must be issued or received")
	}
	f.Kind, f.Status = q.Kind, q.Status
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListInvoices(ctx, f, q.After, limit+1)
	if err != nil {
		return InvoicePage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := InvoicePage{Items: rows}
	if more {
		last := rows[len(rows)-1]
		out.Next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}

// GetInvoice implements Service.
func (s *service) GetInvoice(ctx context.Context, a authz.Actor, id uuid.UUID) (InvoiceView, error) {
	inv, err := s.load(ctx, a, authz.BillingInvoiceRead, id)
	if err != nil {
		return InvoiceView{}, err
	}
	return s.view(ctx, inv)
}

// InvoicePDF implements Service: an issued invoice's PDF for every reader
// (04 §2.18 "Yalnız issued"); before the issue only the uploader side and
// finance (billing.invoice.upload on it) see the uploaded PDF, which the
// verifier must read (409 billing.invalid_state for anyone else).
func (s *service) InvoicePDF(ctx context.Context, a authz.Actor, id uuid.UUID) (files.Download, error) {
	inv, err := s.load(ctx, a, authz.BillingInvoiceRead, id)
	if err != nil {
		return files.Download{}, err
	}
	if inv.PDFFileID == nil {
		return files.Download{}, ErrInvalidState
	}
	if !inv.WasIssued() {
		if err := authz.Can(ctx, a, authz.BillingInvoiceUpload, invoiceResource(inv)); err != nil {
			return files.Download{}, ErrInvalidState
		}
	}
	return s.d.Files.PresignGet(ctx, *inv.PDFFileID, pdfURLTTL)
}

// ---- upload

// UploadInvoice implements Service: authz on the invoice (404 / 403; k29)
// → the input (422) → the PDF (an active invoice_pdf PDF of the actor or of
// the issuer org: 422) → in one transaction: the invoice locked, uploadable
// (planned without a document, or mismatch: 409), authz again on the locked
// row → the PDF attached → the number, date, type, PDF and uploader
// written (the issuer's number taken: 409 billing.invoice_no_taken) →
// audit → finance is asked to verify (A).
func (s *service) UploadInvoice(ctx context.Context, a authz.Actor, id uuid.UUID, in UploadInput) (InvoiceView, error) {
	inv, err := s.load(ctx, a, authz.BillingInvoiceUpload, id)
	if err != nil {
		return InvoiceView{}, err
	}
	no := strings.TrimSpace(in.InvoiceNo)
	issued, derr := civil.Parse(strings.TrimSpace(in.IssueDate))
	today := civil.Today(s.d.Clock.Now())
	switch {
	case !invoiceNoRe.MatchString(no):
		return InvoiceView{}, fieldErr("body.invoice_no", "must be 1-30 letters, digits or / . _ -")
	case derr != nil:
		return InvoiceView{}, fieldErr("body.issue_date", "must be a YYYY-MM-DD date")
	case issued.After(today):
		return InvoiceView{}, fieldErr("body.issue_date", "must not be in the future")
	case !in.DocumentType.Valid():
		return InvoiceView{}, fieldErr("body.document_type", "must be e_fatura, e_arsiv or paper")
	case in.PDFFileID == uuid.Nil():
		return InvoiceView{}, fieldErr("body.pdf_file_id", "is required: an invoice_pdf upload")
	case in.ETTN != nil && *in.ETTN == uuid.Nil():
		return InvoiceView{}, fieldErr("body.ettn", "must be a UUID")
	}
	f, err := s.uploadable(ctx, a, inv, in.PDFFileID)
	if err != nil {
		return InvoiceView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Invoice uuid.UUID
		No      string
		Date    string
		Doc     DocumentType
		File    uuid.UUID
		ETTN    *uuid.UUID
	}{"upload_invoice", id, no, issued.String(), in.DocumentType, in.PDFFileID, in.ETTN})
	if err != nil {
		return InvoiceView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetInvoice(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if !cur.Uploadable() || !cur.IssuingMode.Uploaded() {
			return uuid.Nil(), ErrInvalidState
		}
		if err := authz.Can(ctx, a, authz.BillingInvoiceUpload, invoiceResource(cur)); err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Files.Attach(ctx, a, f.ID, files.Owner{Module: "billing", EntityID: cur.ID, Purpose: files.PurposeInvoicePDF}); err != nil {
			return uuid.Nil(), err
		}
		by, file, doc := a.UserID, f.ID, in.DocumentType
		next := cur
		next.Status, next.DocumentType, next.InvoiceNo, next.ETTN, next.IssueDate = StatusPlanned, doc, &no, in.ETTN, &issued
		next.PDFFileID, next.PDFSHA256, next.UploadedBy = &file, nil, &by
		if len(f.SHA256) == 32 {
			next.PDFSHA256 = f.SHA256
		}
		next.VerifiedBy, next.VerifiedAt, next.LastError = nil, nil, nil
		stored, err := s.d.Store.UpdateInvoice(ctx, next, cur.Status)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, invoiceEvent(a, authz.BillingInvoiceUpload, stored, "", map[string]any{
			"from": string(cur.Status), "document_type": string(doc), "pdf_file_id": file.String(),
		})); err != nil {
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

// uploadable checks the PDF of an upload (422 unless it is an active
// invoice_pdf PDF the actor uploaded or the issuer org owns; 409 when the
// scan found it infected).
func (s *service) uploadable(ctx context.Context, a authz.Actor, inv Invoice, fileID uuid.UUID) (files.FileMeta, error) {
	const loc = "body.pdf_file_id"
	f, err := s.d.Files.Get(ctx, fileID)
	switch {
	case errors.Is(err, files.ErrNotFound):
		return files.FileMeta{}, fieldErr(loc, "must be an invoice_pdf upload")
	case err != nil:
		return files.FileMeta{}, err
	case f.Purpose != files.PurposeInvoicePDF:
		return files.FileMeta{}, fieldErr(loc, "must be an invoice_pdf upload")
	case f.Status != files.StatusActive:
		return files.FileMeta{}, fieldErr(loc, "must be a completed upload")
	case f.MIME != files.MIMEPDF:
		return files.FileMeta{}, fieldErr(loc, "must be a PDF document")
	case f.ScanStatus == files.ScanInfected:
		return files.FileMeta{}, files.ErrNotClean
	}
	mine := f.UploadedBy != nil && *f.UploadedBy == a.UserID
	issuers := inv.IssuerOrgID != nil && f.OrgID != nil && *f.OrgID == *inv.IssuerOrgID
	if !mine && !issuers {
		return files.FileMeta{}, fieldErr(loc, "must be your upload or an upload of the issuing organization")
	}
	return f, nil
}

// ---- verification (four eyes)

// AdminVerifyInvoice implements Service (03 §6.1 "Fatura doğrulama"): the
// admin surface (403 before reading) → the invoice (404), an upload waiting
// (409) → authz on it (403 authz.four_eyes_violation for its uploader; fin
// only) → the input (422) → for issued, the PDF must not be infected (409)
// → in one transaction: the invoice locked, still waiting, the eyes again on
// the locked row → verified by, at; issued (issued_at, invoice.issued: the
// PDF e-mail) or mismatch (the note; the uploader side is told) → audit.
// The database refuses the uploader too (invoices_verifier_chk, 06 H-19).
func (s *service) AdminVerifyInvoice(ctx context.Context, a authz.Actor, id uuid.UUID, in VerifyInput) (InvoiceView, error) {
	if err := authz.Can(ctx, a, authz.BillingInvoiceListAdmin, adminSurface); err != nil {
		return InvoiceView{}, err
	}
	inv, err := s.d.Store.GetInvoice(ctx, id, false)
	if err != nil {
		return InvoiceView{}, err
	}
	if !inv.AwaitsVerification() {
		return InvoiceView{}, ErrInvalidState
	}
	if err := authz.Can(ctx, a, authz.BillingInvoiceVerify, invoiceResource(inv)); err != nil {
		return InvoiceView{}, err
	}
	note := strings.TrimSpace(in.Note)
	switch {
	case in.Result != StatusIssued && in.Result != StatusMismatch:
		return InvoiceView{}, fieldErr("body.result", "must be issued or mismatch")
	case utf8.RuneCountInString(note) > maxNoteRunes:
		return InvoiceView{}, fieldErr("body.note", "must be at most 500 characters")
	case in.Result == StatusMismatch && note == "":
		return InvoiceView{}, fieldErr("body.note", "is required for a mismatch: what does not match")
	}
	if in.Result == StatusIssued {
		f, err := s.d.Files.Get(ctx, *inv.PDFFileID)
		if err != nil {
			return InvoiceView{}, err
		}
		if f.Status != files.StatusActive || f.ScanStatus == files.ScanInfected || f.ScanStatus == files.ScanPending {
			return InvoiceView{}, files.ErrNotClean
		}
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Invoice uuid.UUID
		Result  Status
		Note    string
	}{"verify_invoice", id, in.Result, note})
	if err != nil {
		return InvoiceView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetInvoice(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if !cur.AwaitsVerification() {
			return uuid.Nil(), ErrInvalidState
		}
		if err := authz.Can(ctx, a, authz.BillingInvoiceVerify, invoiceResource(cur)); err != nil {
			return uuid.Nil(), err
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.VerifiedBy, next.VerifiedAt = in.Result, &by, &now
		if in.Result == StatusIssued {
			next.IssuedAt, next.LastError = &now, nil
		} else {
			next.LastError = &note
		}
		stored, err := s.d.Store.UpdateInvoice(ctx, next, cur.Status)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, invoiceEvent(a, authz.BillingInvoiceVerify, stored, note,
			map[string]any{"from": string(cur.Status), "result": string(in.Result)})); err != nil {
			return uuid.Nil(), err
		}
		if in.Result == StatusIssued {
			return stored.ID, s.emitIssued(ctx, stored)
		}
		s.notice(ctx, stored, noticeMismatch)
		return stored.ID, nil
	})
	if err != nil {
		return InvoiceView{}, translate(err)
	}
	return s.reload(ctx, id)
}

// emitIssued writes invoice.issued (O) in the transaction.
func (s *service) emitIssued(ctx context.Context, inv Invoice) error {
	env, err := domainevent.New(domainevent.InvoiceIssued{InvoiceID: inv.ID}, invoiceSubject, inv.ID)
	if err != nil {
		return err
	}
	return s.d.Outbox.Add(ctx, env)
}

// ---- cancellation

// AdminCancelInvoice implements Service: authz on the admin surface (fin;
// 403 before reading) → the reason (422) → in one transaction: the invoice
// locked → an unissued plan is cancelled; an issued invoice is cancelled
// and a credit note of it planned; a cancelled invoice, an issued credit
// note or a document at the provider is 409 → audit.
func (s *service) AdminCancelInvoice(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (CancelResult, error) {
	if err := authz.Can(ctx, a, authz.BillingInvoiceCancel, adminSurface); err != nil {
		return CancelResult{}, err
	}
	why := strings.TrimSpace(reason)
	switch n := utf8.RuneCountInString(why); {
	case n == 0:
		return CancelResult{}, fieldErr("body.reason", "is required")
	case n > maxNoteRunes:
		return CancelResult{}, fieldErr("body.reason", "must be at most 500 characters")
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Invoice uuid.UUID
		Reason  string
	}{"cancel_invoice", id, why})
	if err != nil {
		return CancelResult{}, err
	}
	type ids struct {
		Invoice    uuid.UUID  `json:"invoice"`
		CreditNote *uuid.UUID `json:"credit_note,omitempty"`
	}
	got, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (ids, error) {
		cur, err := s.d.Store.GetInvoice(ctx, id, true)
		if err != nil {
			return ids{}, err
		}
		stored, credit, err := s.cancel(ctx, a, authz.BillingInvoiceCancel, cur, why)
		if err != nil {
			return ids{}, err
		}
		out := ids{Invoice: stored.ID}
		if credit != nil {
			out.CreditNote = &credit.ID
		}
		return out, nil
	})
	if err != nil {
		return CancelResult{}, translate(err)
	}
	inv, err := s.d.Store.GetInvoice(ctx, got.Invoice, false)
	if err != nil {
		return CancelResult{}, err
	}
	res := CancelResult{Invoice: inv}
	if got.CreditNote != nil {
		cn, err := s.d.Store.GetInvoice(ctx, *got.CreditNote, false)
		if err != nil {
			return CancelResult{}, err
		}
		res.CreditNote = &cn
	}
	return res, nil
}

// cancel ends a locked invoice in the caller's transaction and, when it
// was issued, plans its credit note (same parties and amounts, the issuer's
// upload mode; trigger manual).
func (s *service) cancel(ctx context.Context, a authz.Actor, act authz.Action, cur Invoice, why string) (Invoice, *Invoice, error) {
	switch {
	case cur.Status == StatusCancelled, cur.Status == StatusProviderPending:
		return Invoice{}, nil, ErrInvalidState
	case cur.Kind == KindCreditNote && cur.WasIssued():
		return Invoice{}, nil, ErrInvalidState // an issued credit note is final
	}
	next := cur
	next.Status = StatusCancelled
	stored, err := s.d.Store.UpdateInvoice(ctx, next, cur.Status)
	if err != nil {
		return Invoice{}, nil, err
	}
	meta := map[string]any{"from": string(cur.Status)}
	var credit *Invoice
	if cur.WasIssued() {
		x, err := s.creditNoteOf(ctx, cur)
		if err != nil {
			return Invoice{}, nil, err
		}
		cn, _, err := s.d.Store.InsertInvoice(ctx, x.inv, x.lines)
		if err != nil {
			return Invoice{}, nil, err
		}
		credit = &cn
		meta["credit_note_id"] = cn.ID.String()
		if err := s.d.Audit.Log(ctx, invoiceEvent(a, act, cn, why,
			map[string]any{"planned": true, "refers_to_invoice_id": cur.ID.String()})); err != nil {
			return Invoice{}, nil, err
		}
		s.notice(ctx, cn, noticePlanned)
	}
	if err := s.d.Audit.Log(ctx, invoiceEvent(a, act, stored, why, meta)); err != nil {
		return Invoice{}, nil, err
	}
	return stored, credit, nil
}

// creditNoteOf is the planned credit note of an issued invoice: the same
// issuer, recipient, scope and amounts and a mirror of its lines; Koro's
// own invoices are uploaded by finance (manual_upload), the payee's by its
// org (seller_uploaded). Who issues a credit note for a refunded service is
// MM-05's (T-300).
func (s *service) creditNoteOf(ctx context.Context, orig Invoice) (expected, error) {
	lines, err := s.d.Store.InvoiceLines(ctx, orig.ID)
	if err != nil {
		return expected{}, err
	}
	mirror := make([]Line, len(lines))
	for i, l := range lines {
		mirror[i] = Line{OrderLineID: l.OrderLineID, Description: l.Description, Quantity: l.Quantity,
			UnitPriceAmount: l.UnitPriceAmount, NetAmount: l.NetAmount, VATRateBP: l.VATRateBP, VATAmount: l.VATAmount,
			VATExemptionCode: l.VATExemptionCode, VATWithholdingCode: l.VATWithholdingCode, VATWithheldAmount: l.VATWithheldAmount}
	}
	mode := ModeManualUpload
	if orig.IssuingMode == ModeSellerUploaded {
		mode = ModeSellerUploaded
	}
	ref := orig.ID
	return s.document(Invoice{
		OrderID: orig.OrderID, PayoutID: orig.PayoutID, Kind: KindCreditNote, IssuerPartyID: orig.IssuerPartyID,
		RecipientPartyID: orig.RecipientPartyID, IssuerOrgID: orig.IssuerOrgID, RecipientOrgID: orig.RecipientOrgID,
		IssuerSnapshot: orig.IssuerSnapshot, RecipientSnapshot: orig.RecipientSnapshot, IssuingMode: mode,
		Trigger: TriggerManual, RefersToInvoiceID: &ref, IdempotencyKey: key(KindCreditNote, orig.Source(), orig.ID.String()),
	}, mirror)
}

// ---- refunds

// handlePaymentRefunded follows a refund of a whole order line (payment.
// refunded, BE-043): the line's service invoice is cancelled while
// unissued, or cancelled with a credit note once issued. A partial refund,
// an order-level refund (overpayment, a cancelled order) and the fee or
// commission invoices that span several lines are left to finance (logged;
// MM-05, T-300).
func (s *service) handlePaymentRefunded(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PaymentRefunded](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.OrderLineID == uuid.Nil() {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "invoice plan: order-level refund, no invoice change",
			slog.String("order_id", p.OrderID.String()), slog.String("refund_id", p.RefundID.String()))
		return nil
	}
	sys := authz.SystemActor(PlanOnRefundTaskName)
	if err := authz.Can(ctx, sys, authz.BillingInvoicePlan, planResource); err != nil {
		return queue.Permanent(err)
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		o, lines, ok, err := s.billingOrder(ctx, p.OrderID)
		if err != nil || !ok {
			return err
		}
		var line *orders.Line
		for i := range lines {
			if lines[i].ID == p.OrderLineID {
				line = &lines[i]
			}
		}
		if line == nil {
			return queue.Permanent(fmt.Errorf("billing: refunded line %s is not in order %s", p.OrderLineID, p.OrderID))
		}
		if line.RefundedAmount < o.KoroHeld(*line) {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "invoice plan: partial refund of a line, invoices left to finance (MM-05)",
				slog.String("order_line_id", line.ID.String()), slog.String("refund_id", p.RefundID.String()))
			return nil
		}
		invs, err := s.d.Store.InvoicesOfOrderLine(ctx, line.ID)
		if err != nil {
			return err
		}
		reason := "order line refunded (refund " + p.RefundID.String() + ")"
		for _, inv := range invs {
			if inv.Kind != KindServiceSale || inv.Status == StatusCancelled {
				if inv.Kind != KindServiceSale && inv.Status != StatusCancelled {
					s.d.Log.LogAttrs(ctx, slog.LevelWarn, "invoice plan: refunded line on a multi-line invoice, left to finance",
						slog.String("invoice_id", inv.ID.String()), slog.String("order_line_id", line.ID.String()))
				}
				continue
			}
			cur, err := s.d.Store.GetInvoice(ctx, inv.ID, true)
			if err != nil {
				return err
			}
			if _, _, err := s.cancel(ctx, sys, authz.BillingInvoicePlan, cur, reason); err != nil &&
				!errors.Is(err, ErrInvalidState) {
				return err
			}
		}
		return nil
	})
}

// ---- notices (A)

type noticeKind string

const (
	noticePlanned  noticeKind = "planned"
	noticeUploaded noticeKind = "uploaded"
	noticeMismatch noticeKind = "mismatch"
)

// invoiceNotice is the payload of NotifyInvoiceTaskName (IDs only).
type invoiceNotice struct {
	InvoiceID uuid.UUID  `json:"invoice_id"`
	Kind      noticeKind `json:"kind"`
}

// notice queues a notice after the commit; the dedup ID makes a repeated
// call (an idempotent replay) a no-op, a new upload a new notice.
func (s *service) notice(ctx context.Context, inv Invoice, k noticeKind) {
	dedup := "billing." + string(k) + ":" + inv.ID.String()
	if k != noticePlanned && inv.PDFFileID != nil {
		dedup += ":" + inv.PDFFileID.String()
	}
	s.d.Queue.AfterCommit(ctx, NotifyInvoiceTaskName, invoiceNotice{InvoiceID: inv.ID, Kind: k}, dedup, queue.Queue(queue.Default))
}
