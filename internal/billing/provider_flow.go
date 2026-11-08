package billing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/files"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/platform/civil"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// The provider flow (BE-053; 01 §4.21, §6.2 invoice.requested/failed, §7
// billing.poll_jobs; 02 §22.1 queued → provider_pending → issued | failed).
//
// Koro's own invoices (platform fee, commission, a platform-native line's
// service invoice) whose every line's commerce policy says
// invoice_automation 'provider' go through the wired InvoiceProvider
// instead of a finance upload, when no line carries a VAT exemption or
// withholding (those stay uploads until MM-02/PR-03 are answered). The
// payee's own invoices are uploads in any case (PR-01: another legal
// party's account). Credit notes are uploads (PR-02).
//
//  1. The plan reserves the next number of the series (ReserveInvoiceNo)
//     and writes the invoice 'queued' with invoice.requested (O) in its
//     transaction.
//  2. billing.issue_invoice (invoice.requested): the recipient's contact
//     (provider_contact_map, else UpsertContact), the issue date, the
//     draft (CreateDraft: query before create, totals checked), each
//     persisted as it completes; then 'provider_pending' (from here the
//     invoice cannot be cancelled: the official issue may have started),
//     Issue with the document type left to the provider, the job ID
//     persisted and the first poll enqueued. A step that was done is not
//     repeated by a retry.
//  3. billing.poll_jobs: PollJob; pending → the next poll after a delay
//     doubling from 30 s up to 2 h, re-enqueued by the task itself (not an
//     asynq retry: the worker's retry back-off is capped at 6 h); the
//     waits never add up to more than 24 h, then the invoice fails. Done →
//     FetchPDF (not ready: the next poll), the PDF stored (files,
//     invoice_pdf) and attached, the invoice 'issued' with the official
//     number, ETTN, document type and e-document, audited, and
//     invoice.issued (O): the recipient gets the PDF like any invoice.
//  4. A refusal of the provider (ErrProviderInvalid, ErrProviderNotFound, a
//     failed job, no result within 24 h) ends the invoice 'failed' with
//     the provider's scrubbed message (last_error), audited, and
//     invoice.failed (A): finance is told in-app (billing.invoice_failed).
//     Any other error is transient: the task is retried.
const (
	IssueInvoiceTaskName        = "billing.issue_invoice"         // invoice.requested → the provider flow
	PollJobsTaskName            = "billing.poll_jobs"             // the issue job, re-enqueued until done (≤ 24 h)
	NotifyInvoiceFailedTaskName = "billing.notify_invoice_failed" // invoice.failed (A) → finance in-app

	tmplInvoiceFailed = "billing.invoice_failed"

	pollFirst  = 30 * time.Second
	pollCap    = 2 * time.Hour
	pollBudget = 24 * time.Hour

	auditProviderIssue authz.Action = "billing.invoice.provider_issue"
	auditProviderFail  authz.Action = "billing.invoice.provider_fail"

	maxLastErrorRunes = 2000 // invoices_last_error_chk
)

// pollDelay is the wait before poll n (0-based): 30 s doubling, at most 2 h.
func pollDelay(n int) time.Duration {
	d := pollFirst
	for range n {
		if d >= pollCap {
			return pollCap
		}
		d *= 2
	}
	return min(d, pollCap)
}

// pollWaited is the total wait of the first n polls.
func pollWaited(n int) time.Duration {
	var total time.Duration
	for i := range n {
		total += pollDelay(i)
	}
	return total
}

// pollTask is the payload of billing.poll_jobs.
type pollTask struct {
	InvoiceID uuid.UUID `json:"invoice_id"`
}

// ---- plan

// viaProvider reports whether Koro's invoice of the source lines goes
// through the provider: one is wired, every line's policy says 'provider'
// and no invoice line carries a VAT exemption or withholding.
func (s *service) viaProvider(ctx context.Context, src []orders.Line, lines []Line) (bool, error) {
	if s.d.Provider == nil {
		return false, nil
	}
	for _, l := range src {
		t, err := s.d.Offerings.InvoiceTerms(ctx, l.OfferingID, l.CommercePolicyID)
		if err != nil {
			return false, fmt.Errorf("billing: invoice terms of line %s: %w", l.ID, err)
		}
		if t.Automation != catalog.InvoiceProvider {
			return false, nil
		}
	}
	for _, l := range lines {
		if l.VATExemptionCode != nil || l.VATWithholdingCode != nil || l.VATWithheldAmount != 0 {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn,
				"invoice automation 'provider': a line with a VAT exemption or withholding is planned for upload",
				slog.String("order_line_id", ptrString(l.OrderLineID)))
			return false, nil
		}
	}
	return true, nil
}

func ptrString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// ---- billing.issue_invoice

// handleInvoiceRequested runs the provider flow of a queued invoice up to
// its issue job (steps 2 of the flow).
func (s *service) handleInvoiceRequested(ctx context.Context, e domainevent.Envelope) error {
	ev, err := domainevent.Decode[domainevent.InvoiceRequested](e)
	if err != nil {
		return queue.Permanent(err)
	}
	inv, err := s.d.Store.GetInvoice(ctx, ev.InvoiceID, false)
	switch {
	case errors.Is(err, ErrNotFound):
		return queue.Permanent(fmt.Errorf("billing: requested invoice %s not found", ev.InvoiceID))
	case err != nil:
		return err
	case inv.IssuingMode != ModeProvider || (inv.Status != StatusQueued && inv.Status != StatusProviderPending):
		return nil
	case s.d.Provider == nil || inv.Provider != s.d.ProviderName:
		s.d.Log.LogAttrs(ctx, slog.LevelError, "provider invoice waits: its provider is not wired",
			slog.String("invoice_id", inv.ID.String()), slog.String("provider", inv.Provider), slog.Bool("alarm", true))
		return fmt.Errorf("billing: invoice %s is for provider %q, which is not wired", inv.ID, inv.Provider)
	}
	return s.providerOutcome(ctx, inv.ID, s.issueAtProvider(ctx, inv))
}

// providerOutcome fails the invoice on a refusal of the provider; any
// other error is returned (retried).
func (s *service) providerOutcome(ctx context.Context, id uuid.UUID, err error) error {
	if errors.Is(err, ErrProviderInvalid) || errors.Is(err, ErrProviderNotFound) {
		return s.failProvider(ctx, id, err.Error())
	}
	return err
}

// issueAtProvider drafts a queued invoice, makes it provider_pending,
// starts the issue and enqueues the first poll.
func (s *service) issueAtProvider(ctx context.Context, inv Invoice) error {
	id := inv.ID
	if inv.Status == StatusProviderPending && inv.ProviderJobID != nil {
		return s.schedulePoll(ctx, id, inv.Attempts) // a retry after the job was stored
	}
	if inv.Status == StatusQueued {
		var err error
		if inv, err = s.providerDraft(ctx, inv); err != nil {
			return s.unlessCancelled(ctx, id, err)
		}
		inv.Status = StatusProviderPending
		if inv, err = s.d.Store.UpdateInvoice(ctx, inv, StatusQueued); err != nil {
			return s.unlessCancelled(ctx, id, err)
		}
	}
	if inv.ProviderInvoiceID == nil {
		return fmt.Errorf("%w: invoice %s has no draft at the provider", ErrProviderInvalid, id)
	}
	job, err := s.d.Provider.Issue(ctx, ProviderRef{InvoiceID: *inv.ProviderInvoiceID}, DocUnknown)
	if err != nil {
		return err
	}
	inv.ProviderJobID, inv.Attempts = &job, 0
	if _, err := s.d.Store.UpdateInvoice(ctx, inv, StatusProviderPending); err != nil {
		return s.unlessCancelled(ctx, id, err)
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "invoice issue started at the provider", slog.String("invoice_id", inv.ID.String()),
		slog.String("provider", inv.Provider))
	return s.schedulePoll(ctx, inv.ID, 0)
}

// unlessCancelled ends the flow quietly when the invoice left its state
// meanwhile (cancelled by finance or a refund).
func (s *service) unlessCancelled(ctx context.Context, id uuid.UUID, err error) error {
	if errors.Is(err, ErrInvalidState) {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "provider flow stopped: the invoice changed state",
			slog.String("invoice_id", id.String()))
		return nil
	}
	return err
}

// providerDraft completes the contact, the issue date and the draft of a
// queued invoice, persisting each.
func (s *service) providerDraft(ctx context.Context, inv Invoice) (Invoice, error) {
	if inv.ReservedSeries == nil || inv.ReservedNo == nil {
		return inv, fmt.Errorf("%w: invoice %s has no reserved series and number", ErrProviderInvalid, inv.ID)
	}
	if inv.ProviderContactID == nil || inv.IssueDate == nil {
		id, err := s.providerContact(ctx, inv)
		if err != nil {
			return inv, err
		}
		inv.ProviderContactID = &id
		if inv.IssueDate == nil {
			today := civil.Today(s.d.Clock.Now())
			inv.IssueDate = &today
		}
		if inv, err = s.d.Store.UpdateInvoice(ctx, inv, StatusQueued); err != nil {
			return inv, err
		}
	}
	if inv.ProviderInvoiceID != nil {
		return inv, nil
	}
	lines, err := s.d.Store.InvoiceLines(ctx, inv.ID)
	if err != nil {
		return inv, err
	}
	draft := DraftInvoice{InvoiceID: inv.ID.String(), ContactID: *inv.ProviderContactID, Recipient: inv.RecipientSnapshot,
		Series: *inv.ReservedSeries, No: *inv.ReservedNo, IssueDate: *inv.IssueDate, Currency: inv.Currency,
		TotalGross: inv.TotalAmount}
	for _, l := range lines {
		draft.Lines = append(draft.Lines, DraftLine{Description: l.Description, Quantity: l.Quantity,
			UnitPrice: l.UnitPriceAmount, VATRateBP: l.VATRateBP})
	}
	ref, err := s.d.Provider.CreateDraft(ctx, draft)
	if err != nil {
		return inv, err
	}
	inv.ProviderInvoiceID = &ref.InvoiceID
	return s.d.Store.UpdateInvoice(ctx, inv, StatusQueued)
}

// providerContact returns the recipient's contact at the provider: the
// mapped one, else UpsertContact with the invoice's recipient snapshot
// (its legal type and e-Fatura flag from the party's current record).
func (s *service) providerContact(ctx context.Context, inv Invoice) (string, error) {
	id, err := s.d.Store.ProviderContact(ctx, inv.RecipientPartyID, inv.Provider)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	p := PartySnapshot{Snapshot: inv.RecipientSnapshot}
	party, err := s.d.Parties.Get(ctx, inv.RecipientPartyID)
	switch {
	case err == nil:
		p.LegalType = string(party.LegalType)
		p.EInvoice = party.EInvoiceAlias != nil && *party.EInvoiceAlias != ""
	case !errors.Is(err, ErrNotFound):
		return "", fmt.Errorf("billing: recipient of invoice %s: %w", inv.ID, err)
	}
	if id, err = s.d.Provider.UpsertContact(ctx, p); err != nil {
		return "", err
	}
	if err := s.d.Store.PutProviderContact(ctx, inv.RecipientPartyID, inv.Provider, id); err != nil {
		return "", err
	}
	return id, nil
}

// schedulePoll enqueues poll n of an invoice after pollDelay(n); the dedup
// ID makes a repeated scheduling a no-op.
func (s *service) schedulePoll(ctx context.Context, id uuid.UUID, n int) error {
	return s.d.Queue.Enqueue(ctx, PollJobsTaskName, pollTask{InvoiceID: id}, fmt.Sprintf("%s:%s:%d", PollJobsTaskName, id, n),
		queue.Queue(queue.Default), queue.ProcessIn(pollDelay(n)), queue.MaxRetry(queue.MoneyMaxRetry))
}

// ---- billing.poll_jobs

// handlePollJob polls the issue job of a provider_pending invoice (step 3).
func (s *service) handlePollJob(ctx context.Context, payload []byte) error {
	var t pollTask
	if err := json.Unmarshal(payload, &t); err != nil || t.InvoiceID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("billing: poll payload: %w", errors.Join(err, errors.New("invoice_id required"))))
	}
	inv, err := s.d.Store.GetInvoice(ctx, t.InvoiceID, false)
	switch {
	case errors.Is(err, ErrNotFound):
		return queue.Permanent(fmt.Errorf("billing: polled invoice %s not found", t.InvoiceID))
	case err != nil:
		return err
	case inv.Status != StatusProviderPending || inv.ProviderJobID == nil:
		return nil
	case s.d.Provider == nil:
		return errors.New("billing: the invoice provider is not wired")
	}
	st, err := s.d.Provider.PollJob(ctx, *inv.ProviderJobID)
	if err != nil {
		return s.providerOutcome(ctx, inv.ID, err)
	}
	switch st.State {
	case JobFailed:
		return s.failProvider(ctx, inv.ID, st.Error)
	case JobDone:
		pdf, err := s.d.Provider.FetchPDF(ctx, st.Ref)
		switch {
		case errors.Is(err, ErrProviderNotReady):
			return s.nextPoll(ctx, inv)
		case err != nil:
			return s.providerOutcome(ctx, inv.ID, err)
		}
		return s.issueFromProvider(ctx, inv, st, pdf)
	}
	return s.nextPoll(ctx, inv)
}

// nextPoll schedules the next poll, or fails the invoice once the waits
// would pass 24 hours.
func (s *service) nextPoll(ctx context.Context, inv Invoice) error {
	next := inv.Attempts + 1
	if pollWaited(next+1) > pollBudget {
		return s.failProvider(ctx, inv.ID, "no result from the provider within 24 hours")
	}
	if err := s.schedulePoll(ctx, inv.ID, next); err != nil {
		return err
	}
	inv.Attempts = next
	_, err := s.d.Store.UpdateInvoice(ctx, inv, StatusProviderPending)
	return s.unlessCancelled(ctx, inv.ID, err)
}

// issueFromProvider stores the PDF and issues the invoice with the
// provider's number in one transaction (invoice.issued in it).
func (s *service) issueFromProvider(ctx context.Context, inv Invoice, st JobStatus, pdf []byte) error {
	named := inv
	named.InvoiceNo = &st.InvoiceNo
	fileID, err := s.d.Files.PutGenerated(ctx, files.GeneratedFile{Purpose: files.PurposeInvoicePDF, OrgID: inv.RecipientOrgID,
		Name: pdfName(named), ContentType: files.MIMEPDF, Content: pdf})
	if err != nil {
		return fmt.Errorf("billing: store the PDF of invoice %s: %w", inv.ID, err)
	}
	sum := sha256.Sum256(pdf)
	sys := authz.SystemActor(PollJobsTaskName)
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.GetInvoice(ctx, inv.ID, true)
		if err != nil {
			return err
		}
		if cur.Status != StatusProviderPending {
			return nil // the unattached file goes with files.orphan_cleanup
		}
		if err := s.d.Files.Attach(ctx, sys, fileID, files.Owner{Module: "billing", EntityID: cur.ID,
			Purpose: files.PurposeInvoicePDF}); err != nil {
			return err
		}
		next := cur
		no, now := st.InvoiceNo, s.now()
		next.Status, next.InvoiceNo, next.PDFFileID, next.PDFSHA256, next.IssuedAt, next.LastError =
			StatusIssued, &no, &fileID, sum[:], &now, nil
		next.DocumentType = DocUnknown
		if st.Doc == DocEFatura || st.Doc == DocEArsiv {
			next.DocumentType = st.Doc
		}
		if ettn, err := uuid.Parse(st.ETTN); err == nil {
			next.ETTN = &ettn
		}
		if next.IssueDate == nil {
			today := civil.Today(now)
			next.IssueDate = &today
		}
		if edoc := st.Ref.EDocID; edoc != "" {
			next.ProviderEDocID = &edoc
		}
		stored, err := s.d.Store.UpdateInvoice(ctx, next, StatusProviderPending)
		if err != nil {
			return err
		}
		if err := s.d.Audit.Log(ctx, invoiceEvent(sys, auditProviderIssue, stored, "",
			map[string]any{"provider": stored.Provider, "document_type": string(stored.DocumentType), "attempts": stored.Attempts})); err != nil {
			return err
		}
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "invoice issued by the provider", slog.String("invoice_id", stored.ID.String()),
			slog.String("provider", stored.Provider))
		return s.emitIssued(ctx, stored)
	})
}

// failProvider ends a queued or provider_pending invoice 'failed' with the
// provider's (scrubbed) reason and tells finance (invoice.failed, A).
func (s *service) failProvider(ctx context.Context, id uuid.UUID, reason string) error {
	if utf8.RuneCountInString(reason) > maxLastErrorRunes {
		reason = string([]rune(reason)[:maxLastErrorRunes])
	}
	sys := authz.SystemActor(IssueInvoiceTaskName)
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.GetInvoice(ctx, id, true)
		if err != nil {
			return err
		}
		if cur.Status != StatusQueued && cur.Status != StatusProviderPending {
			return nil
		}
		next := cur
		next.Status, next.LastError = StatusFailed, &reason
		stored, err := s.d.Store.UpdateInvoice(ctx, next, cur.Status)
		if err != nil {
			return err
		}
		if err := s.d.Audit.Log(ctx, invoiceEvent(sys, auditProviderFail, stored, "",
			map[string]any{"provider": stored.Provider, "from": string(cur.Status), "attempts": stored.Attempts})); err != nil {
			return err
		}
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "invoice failed at the provider", slog.String("invoice_id", id.String()),
			slog.String("provider", stored.Provider), slog.Int("attempts", stored.Attempts))
		return outbox.EmitAfterCommit(ctx, s.d.Outbox, domainevent.InvoiceFailed{InvoiceID: id, Attempts: stored.Attempts},
			invoiceSubject, id)
	})
}

// ---- billing.notify_invoice_failed

// handleInvoiceFailed tells finance in-app that a provider invoice failed.
func (s *service) handleInvoiceFailed(ctx context.Context, e domainevent.Envelope) error {
	ev, err := domainevent.Decode[domainevent.InvoiceFailed](e)
	if err != nil {
		return queue.Permanent(err)
	}
	inv, err := s.d.Store.GetInvoice(ctx, ev.InvoiceID, false)
	switch {
	case errors.Is(err, ErrNotFound):
		return queue.Permanent(fmt.Errorf("billing: failed invoice %s not found", ev.InvoiceID))
	case err != nil:
		return err
	case inv.Status != StatusFailed:
		return nil
	case s.d.Sender == nil || s.d.Staff == nil:
		return errors.New("billing: the invoice notices are not wired")
	}
	to, err := s.d.Staff.FinanceAdmins(ctx)
	if err != nil {
		return fmt.Errorf("billing: notice recipients: %w", err)
	}
	return s.notifyInApp(ctx, inv, tmplInvoiceFailed, inv.ID.String(), to, notify.AdminInvoiceLink(inv.ID))
}
