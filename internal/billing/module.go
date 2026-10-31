// Package billing is the invoice records of Koro (01 §4.21; BE-042): the
// invoices a commerce model expects, planned from the money events, the
// upload of the documents and their four-eyes verification, cancellation
// with a credit note, the InvoiceProvider contract of e-invoice adapters
// (Paraşüt: BE-053) and the integration credentials.
//
// Rules the package enforces:
//
//   - The plan (02 §18.3 table; system action billing.invoice.plan) is
//     idempotent: every expected document has its key
//     "<kind>:<order_id|payout_id>:<order_line_id|all>:<seq>" (unique in
//     invoices), so a repeated event plans nothing twice. T-11 (who invoices
//     whom) is not decided; until it is, model A is planned: per order line
//     the payee's service invoice to the buyer (seller_uploaded) when the
//     offering's invoice_trigger fires (payment.captured of the full payment
//     or order_line.fulfilled), Koro's own service invoice for a
//     platform-native line, Koro's platform fee invoice to the buyer when the
//     buyer pays the commission, and on payout.paid Koro's commission invoice
//     to the payee for the paid-out lines whose seller pays the commission
//     (manual_upload). Model C plans only Koro's fee to the buyer (its
//     buyer_fee_amount). Models A2 and B plan nothing (logged; T-299).
//     invoice_automation 'provider' is still planned for upload until BE-053
//     wires the provider flow (logged).
//   - Upload (billing.invoice.upload; 03 k29): a planned invoice without a
//     document, or one found mismatched, takes the PDF (an invoice_pdf
//     upload of the actor or of the issuer org, application/pdf), the number,
//     the date and the document type; the file is attached in the same
//     transaction. The issuer's org (club admin, university rep) uploads its
//     own service invoices only; finance uploads any.
//   - Verification (billing.invoice.verify, four eyes; 03 §6.1, 06 H-19):
//     a finance admin other than the uploader confirms the number, date and
//     amounts → issued (invoice.issued: the PDF is e-mailed to the
//     recipient) or mismatch (the uploader side is told, may upload again).
//     The uploader is refused by authz, again on the locked row, and by the
//     database CHECK invoices_verifier_chk.
//   - Cancellation (billing.invoice.cancel): an unissued plan is cancelled;
//     an issued invoice is cancelled and a credit note of it is planned. An
//     issued credit note is final (409).
//   - A refund of a whole order line (payment.refunded) cancels the line's
//     unissued service invoice and plans a credit note of an issued one; a
//     partial refund is logged for finance (MM-05).
//   - Integration credentials (billing.integration.manage, R) are sealed
//     with the shared envelope (BE-055) and never returned.
//   - Org isolation (06 H-38): an invoice's scope columns (issuer and
//     recipient org) come from the order, the payee and the payout; the org
//     surface lists the org's issued and received invoices only.
package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// Service is the billing service: the HTTP use cases of 04 §2.18 (billing
// offers no cross-module contract; the money events drive it). Every method
// calls authz.Can first (05 §5.3).
type Service interface {
	// ListOrgInvoices is listOrgInvoices (billing.invoice.list): the org's
	// issued and/or received invoices, newest first.
	ListOrgInvoices(ctx context.Context, a authz.Actor, orgID uuid.UUID, q ListQuery) (InvoicePage, error)
	// GetInvoice is getInvoice (billing.invoice.read).
	GetInvoice(ctx context.Context, a authz.Actor, id uuid.UUID) (InvoiceView, error)
	// InvoicePDF is downloadInvoicePdf (billing.invoice.read): a 5-minute
	// URL of an issued invoice's PDF (an unissued upload: its uploader side
	// and finance only).
	InvoicePDF(ctx context.Context, a authz.Actor, id uuid.UUID) (files.Download, error)
	// UploadInvoice is uploadInvoice (billing.invoice.upload).
	UploadInvoice(ctx context.Context, a authz.Actor, id uuid.UUID, in UploadInput) (InvoiceView, error)
	// AdminListInvoices is adminListInvoices (billing.invoice.list_admin).
	AdminListInvoices(ctx context.Context, a authz.Actor, q ListQuery) (InvoicePage, error)
	// ReplaceInvoicePDF is replaceInvoicePdf (billing.invoice.upload): a
	// rejected PDF uploaded again (BE-154b).
	ReplaceInvoicePDF(ctx context.Context, a authz.Actor, id, fileID uuid.UUID) (InvoiceView, error)
	// AdminVerifyInvoice is adminVerifyInvoice (billing.invoice.verify: four eyes).
	AdminVerifyInvoice(ctx context.Context, a authz.Actor, id uuid.UUID, in VerifyInput) (InvoiceView, error)
	// AdminCancelInvoice is adminCancelInvoice (billing.invoice.cancel); the
	// result carries the credit note planned for an issued invoice.
	AdminCancelInvoice(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (CancelResult, error)
	// AdminListIntegrations is adminListIntegrations
	// (billing.integration.manage): no secret value.
	AdminListIntegrations(ctx context.Context, a authz.Actor) ([]Integration, error)
	// AdminPutIntegration is adminPutIntegration (billing.integration.manage,
	// R): the tokens are sealed, never returned.
	AdminPutIntegration(ctx context.Context, a authz.Actor, provider string, in IntegrationInput, proof identity.ReauthProof) (Integration, error)
}

// Deps are the dependencies of the billing module. Store, Tx, Clock, Log,
// Audit, Outbox, Queue, Idempotency, Orders, Offerings, Parties, Payees,
// Orgs and Files are required (New fails otherwise). Reauth nil →
// adminPutIntegration 503; Sealer nil → adminPutIntegration 503 (nothing
// is stored in clear); Sender or Staff nil → the notice task fails and is
// retried. Provider is the e-invoice adapter of BE-053 (nil: every invoice
// is uploaded).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Clock       clock.Clock
	Log         *slog.Logger
	Audit       AuditLogger
	Outbox      outbox.Writer       // invoice.issued (O) in the transaction
	Queue       queue.Enqueuer      // the notices (A, after the commit)
	Idempotency *idempotency.Keeper // Idempotency-Key of the POSTs (optional, 06 H-5)
	Orders      Orders
	Offerings   Offerings
	Parties     Parties
	Payees      Payees
	Orgs        Orgs
	Files       Files

	Reauth   Reauthenticator
	Sealer   Sealer
	Sender   Sender
	Staff    Staff
	Provider InvoiceProvider
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
		{"Audit", d.Audit != nil}, {"Outbox", d.Outbox != nil}, {"Queue", d.Queue != nil},
		{"Idempotency", d.Idempotency != nil}, {"Orders", d.Orders != nil}, {"Offerings", d.Offerings != nil},
		{"Parties", d.Parties != nil}, {"Payees", d.Payees != nil}, {"Orgs", d.Orgs != nil}, {"Files", d.Files != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("billing.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what billing hands to the composition root.
type Module struct {
	Service Service
	// Privacy is the KVKK provider of the invoices (the staff and org
	// admins who uploaded or verified them; financial records, retained).
	Privacy privacy.Provider

	svc *service
}

type service struct{ d Deps }

// New builds the billing module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Privacy: privacyProvider{s: s}, svc: s}, nil
}
