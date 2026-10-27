package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/money"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Templates of the invoice notices (internal/notify/templates/billing).
// In-app texts are fixed (no data): the deep link carries the invoice.
const (
	tmplInvoiceIssued   = "billing.invoice_issued"   // the recipient: the PDF (e-mail) and its org's admins (in-app)
	tmplInvoicePlanned  = "billing.invoice_planned"  // the uploaders: the issuer org's admins or finance
	tmplInvoiceUploaded = "billing.invoice_uploaded" // finance (but the uploader): verify it
	tmplInvoiceMismatch = "billing.invoice_mismatch" // the uploader and the issuer org's admins
)

// orgAdminRoles are the roles of an org that hear about its invoices: the
// club admins and university reps of a seller, the admins and owner of a
// buyer company.
var orgAdminRoles = []authz.MemberRole{authz.MemberClubAdmin, authz.MemberUniversityRep, authz.MemberCompanyAdmin,
	authz.MemberCompanyOwner}

// Subscribe registers the plan consumers and the invoice e-mail (01 §6.2):
// payment.captured, order_line.fulfilled, payout.paid and payment.refunded
// → the plan; invoice.issued → the PDF to the recipient. Money handlers run
// on the critical queue with the money retries (01 §8).
func (m Module) Subscribe(r *outbox.Registry) {
	opts := []outbox.Option{outbox.Queue(queue.Critical), outbox.MaxRetry(queue.MoneyMaxRetry)}
	r.Subscribe(domainevent.TopicPaymentCaptured, PlanOnPaymentTaskName, m.svc.handlePaymentCaptured, opts...)
	r.Subscribe(domainevent.TopicOrderLineFulfilled, PlanOnFulfillmentTaskName, m.svc.handleLineFulfilled, opts...)
	r.Subscribe(domainevent.TopicPayoutPaid, PlanOnPayoutTaskName, m.svc.handlePayoutPaid, opts...)
	r.Subscribe(domainevent.TopicPaymentRefunded, PlanOnRefundTaskName, m.svc.handlePaymentRefunded, opts...)
	r.Subscribe(domainevent.TopicInvoiceIssued, SendInvoiceTaskName, m.svc.handleInvoiceIssued, opts...)
}

// Tasks returns the plain task handlers of billing: the A notices.
func (m Module) Tasks() []queue.Task {
	return []queue.Task{{Name: NotifyInvoiceTaskName, Handle: m.svc.handleNotice}}
}

// handleInvoiceIssued e-mails the issued invoice's PDF to the recipient
// party's address (current, else the snapshot's; none → in-app only,
// logged) and tells the recipient org's admins in-app. A PDF that is not
// cleared by the scan yet fails the task (retried).
func (s *service) handleInvoiceIssued(ctx context.Context, e domainevent.Envelope) error {
	ev, err := domainevent.Decode[domainevent.InvoiceIssued](e)
	if err != nil {
		return queue.Permanent(err)
	}
	inv, err := s.d.Store.GetInvoice(ctx, ev.InvoiceID, false)
	switch {
	case errors.Is(err, ErrNotFound):
		return queue.Permanent(fmt.Errorf("billing: issued invoice %s not found", ev.InvoiceID))
	case err != nil:
		return err
	case inv.Status != StatusIssued || inv.PDFFileID == nil:
		return nil // cancelled before the e-mail: nothing to send
	}
	if s.d.Sender == nil {
		return errors.New("billing: notify is not wired")
	}
	errs := []error{s.mailInvoice(ctx, inv)}
	if inv.RecipientOrgID != nil {
		admins, err := s.orgAdmins(ctx, *inv.RecipientOrgID)
		if err != nil {
			return err
		}
		errs = append(errs, s.notifyInApp(ctx, inv, tmplInvoiceIssued, e.ID.String(), admins,
			notify.OrgInvoiceLink(*inv.RecipientOrgID, inv.ID)))
	}
	return errors.Join(errs...)
}

// mailInvoice sends the PDF to the recipient's invoice address.
func (s *service) mailInvoice(ctx context.Context, inv Invoice) error {
	to := inv.RecipientSnapshot.Email
	if p, err := s.d.Parties.Get(ctx, inv.RecipientPartyID); err == nil && p.Email != nil && *p.Email != "" {
		to = *p.Email
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("billing: recipient of invoice %s: %w", inv.ID, err)
	}
	if to == "" {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "invoice e-mail not sent: the recipient has no invoice address",
			slog.String("invoice_id", inv.ID.String()))
		return nil
	}
	pdf, _, err := s.d.Files.Read(ctx, *inv.PDFFileID, maxPDFBytes)
	if err != nil {
		return fmt.Errorf("billing: PDF of invoice %s: %w", inv.ID, err)
	}
	no, date := "", ""
	if inv.InvoiceNo != nil {
		no = *inv.InvoiceNo
	}
	if inv.IssueDate != nil {
		date = inv.IssueDate.String()
	}
	org := inv.IssuerOrgID
	if org == nil {
		org = inv.RecipientOrgID
	}
	_, err = s.d.Sender.Send(ctx, notify.Message{
		ToEmail: to, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional, Template: tmplInvoiceIssued,
		Locale: "tr", SourceOrgID: org, DedupeKey: tmplInvoiceIssued + ":" + inv.ID.String() + ":email",
		Data: map[string]any{
			"issuer_title": inv.IssuerSnapshot.Title, "recipient_title": inv.RecipientSnapshot.Title, "invoice_no": no,
			"issue_date": date, "total_amount": money.Decimal(inv.TotalAmount), "currency": inv.Currency,
		},
		Attachments: []notify.Attachment{{Filename: pdfName(inv), ContentType: "application/pdf", Data: pdf}},
	})
	return s.sendErr(ctx, tmplInvoiceIssued, inv.ID, err)
}

// pdfName is the attachment's file name: the invoice number when it is a
// safe file name, else the invoice ID.
func pdfName(inv Invoice) string {
	if inv.InvoiceNo != nil && invoiceNoRe.MatchString(*inv.InvoiceNo) {
		return "fatura-" + strings.ReplaceAll(*inv.InvoiceNo, "/", "-") + ".pdf"
	}
	return "fatura-" + inv.ID.String() + ".pdf"
}

// handleNotice sends the A notices of the manual flow: a planned invoice
// to its uploaders (the issuer org's admins for the payee's invoice,
// finance for Koro's), an upload to finance (but the uploader) for the
// verification, a mismatch to the uploader and the issuer org's admins.
func (s *service) handleNotice(ctx context.Context, payload []byte) error {
	var n invoiceNotice
	if err := json.Unmarshal(payload, &n); err != nil || n.InvoiceID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("billing: invoice notice payload: %w", errors.Join(err, errors.New("invoice_id required"))))
	}
	inv, err := s.d.Store.GetInvoice(ctx, n.InvoiceID, false)
	switch {
	case errors.Is(err, ErrNotFound):
		return queue.Permanent(fmt.Errorf("billing: invoice notice: invoice %s not found", n.InvoiceID))
	case err != nil:
		return err
	}
	if s.d.Sender == nil || s.d.Staff == nil {
		return errors.New("billing: the invoice notices are not wired")
	}
	key := string(n.Kind) + ":" + inv.ID.String()
	if inv.PDFFileID != nil && n.Kind != noticePlanned {
		key += ":" + inv.PDFFileID.String()
	}
	var to []uuid.UUID
	tmpl := ""
	// Finance opens the invoice in the admin panel; an org's admins in their
	// org's panel (BE-144: the link names the org).
	link := notify.AdminInvoiceLink(inv.ID)
	switch n.Kind {
	case noticePlanned:
		if inv.Status != StatusPlanned {
			return nil
		}
		tmpl = tmplInvoicePlanned
		if inv.IssuingMode == ModeSellerUploaded && inv.IssuerOrgID != nil {
			to, err = s.orgAdmins(ctx, *inv.IssuerOrgID)
			link = notify.OrgInvoiceLink(*inv.IssuerOrgID, inv.ID)
		} else {
			to, err = s.d.Staff.FinanceAdmins(ctx)
		}
	case noticeUploaded:
		if !inv.AwaitsVerification() {
			return nil
		}
		tmpl = tmplInvoiceUploaded
		to, err = s.d.Staff.FinanceAdmins(ctx)
		to = slices.DeleteFunc(slices.Clone(to), func(id uuid.UUID) bool { return id == *inv.UploadedBy })
	case noticeMismatch:
		if inv.Status != StatusMismatch {
			return nil
		}
		tmpl = tmplInvoiceMismatch
		if inv.IssuerOrgID != nil {
			to, err = s.orgAdmins(ctx, *inv.IssuerOrgID)
			link = notify.OrgInvoiceLink(*inv.IssuerOrgID, inv.ID)
		}
		if inv.UploadedBy != nil && !slices.Contains(to, *inv.UploadedBy) {
			to = append(to, *inv.UploadedBy)
		}
	default:
		return queue.Permanent(fmt.Errorf("billing: unknown invoice notice %q", n.Kind))
	}
	if err != nil {
		return fmt.Errorf("billing: notice recipients: %w", err)
	}
	return s.notifyInApp(ctx, inv, tmpl, key, to, link)
}

// orgAdmins returns an org's active admins (orgAdminRoles).
func (s *service) orgAdmins(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	ids, err := s.d.Orgs.ActiveMemberIDs(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("billing: org members: %w", err)
	}
	var out []uuid.UUID
	for _, id := range ids {
		ok, err := s.d.Orgs.HasRole(ctx, orgID, id, orgAdminRoles...)
		if err != nil {
			return nil, fmt.Errorf("billing: org member role: %w", err)
		}
		if ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// notifyInApp sends an in-app notice (IDs only) linking link to every
// recipient; the dedupe key (the event and the recipient) makes a repeated
// task a no-op.
func (s *service) notifyInApp(ctx context.Context, inv Invoice, tmpl, key string, to []uuid.UUID, link notify.Link) error {
	if len(to) == 0 {
		return nil
	}
	org := inv.IssuerOrgID
	if org == nil {
		org = inv.RecipientOrgID
	}
	var errs []error
	for _, uid := range to {
		u := uid
		_, err := s.d.Sender.Send(ctx, notify.Message{
			UserID: &u, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmpl,
			SourceOrgID: org, LinkPath: link, DedupeKey: tmpl + ":" + key + ":" + u.String(),
			Data: map[string]any{"invoice_id": inv.ID.String()},
		})
		errs = append(errs, s.sendErr(ctx, tmpl, inv.ID, err))
	}
	return errors.Join(errs...)
}

// sendErr keeps a retryable send error and logs a permanent one.
func (s *service) sendErr(ctx context.Context, tmpl string, invoiceID uuid.UUID, err error) error {
	switch {
	case err == nil:
		return nil
	case notify.IsPermanent(err):
		s.d.Log.LogAttrs(ctx, slog.LevelError, "invoice notice not deliverable", slog.String("template", tmpl),
			slog.String("invoice_id", invoiceID.String()), slog.String("err", err.Error()))
		return nil
	}
	return err
}
