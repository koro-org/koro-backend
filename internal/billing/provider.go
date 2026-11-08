package billing

import (
	"context"
	"fmt"

	"koro/internal/platform/civil"
	"koro/internal/platform/money"
)

// InvoiceProvider is the adapter contract of an e-invoice provider (01
// §4.21: Paraşüt, later Nilvera), used when a commerce policy's
// invoice_automation is 'provider'. Adapters (chosen by
// BILLING_INVOICE_PROVIDER, cmd/koro/wire_billing.go): ParasutProvider
// (parasut.go, BE-053) and FakeProvider (mem: tests and development).
// providerContract (provider_contract_test.go) is the suite every adapter
// passes; the provider flow (provider_flow.go) drives it.
//
// Every call is idempotent from the caller's side (asynq retries them):
//
//   - UpsertContact keys the contact by the party's tax number: the same
//     party twice is the same contact ID.
//   - CreateDraft looks up (Series, No) first and creates only when the
//     provider has no draft with them ("sorgu-önce-oluştur"). A draft whose
//     totals differ from Koro's is ErrProviderInvalid: it is never issued.
//   - Issue on an already issued (or issuing) draft returns its job.
//
// Errors wrap ErrProviderNotFound (unknown reference or job),
// ErrProviderNotReady (FetchPDF before the document is issued) or
// ErrProviderInvalid (the provider rejected the request: not retried); any
// other error is transient (the provider unreachable, throttled or failing,
// a credential problem) and retried. Errors never carry a token or the
// parties' data.
type InvoiceProvider interface {
	// UpsertContact creates or updates the party at the provider and
	// returns its contact ID (provider_contact_map).
	UpsertContact(ctx context.Context, p PartySnapshot) (contactID string, err error)
	// CreateDraft returns the provider's draft of in, creating it unless a
	// draft with in.Series and in.No exists.
	CreateDraft(ctx context.Context, in DraftInvoice) (ProviderRef, error)
	// Issue starts the official issue of the draft as doc (e-Fatura or
	// e-Arşiv; DocUnknown: the provider picks e-Fatura when the recipient
	// is registered for it, else e-Arşiv) and returns an opaque job ID.
	Issue(ctx context.Context, ref ProviderRef, doc DocumentType) (jobID string, err error)
	// PollJob returns the state of an issue job.
	PollJob(ctx context.Context, jobID string) (JobStatus, error)
	// FetchPDF returns the PDF of an issued document.
	FetchPDF(ctx context.Context, ref ProviderRef) ([]byte, error)
}

// PartySnapshot is a legal party as the provider's contact (no national ID).
type PartySnapshot struct {
	Snapshot
	LegalType string // legal_parties.legal_type
	EInvoice  bool   // the party is registered for e-Fatura (alias known)
}

// DraftInvoice is what CreateDraft sends: Koro's reserved series and number
// (invoices.reserved_series/no) make the call idempotent.
type DraftInvoice struct {
	InvoiceID  string // Koro's invoice ID, kept as the provider's external reference
	ContactID  string
	Recipient  Snapshot // the recipient as the invoice names it (title, tax office and number, address)
	Series     string
	No         int
	IssueDate  civil.Date
	Currency   string
	Lines      []DraftLine
	TotalGross int64 // kuruş: Σ net + VAT, checked by the provider
}

// DraftLine is one line of a draft (kuruş, VAT in basis points).
type DraftLine struct {
	Description string
	Quantity    int
	UnitPrice   int64
	VATRateBP   int
}

// ProviderRef identifies a document at the provider (invoices.
// provider_invoice_id; provider_edoc_id once issued).
type ProviderRef struct {
	InvoiceID string
	EDocID    string
}

// JobState is the state of an issue job.
type JobState string

// Job states.
const (
	JobPending JobState = "pending"
	JobDone    JobState = "done"
	JobFailed  JobState = "failed"
)

// JobStatus is the answer of PollJob.
type JobStatus struct {
	State     JobState
	Ref       ProviderRef  // EDocID set once done
	Doc       DocumentType // the document issued (e-Fatura or e-Arşiv) once done
	InvoiceNo string       // the official number once done
	ETTN      string       // the document's UUID once done ("" when the provider gives none)
	Error     string       // the provider's message when failed (no personal data)
}

// checkDraft is what every adapter checks before a call: the series, the
// number, the issue date and the lines are present and the lines sum to
// TotalGross (Σ quantity × unit price + VAT, platform/money). It returns the
// draft's VAT total; a failed check is ErrProviderInvalid.
func checkDraft(in DraftInvoice) (vat int64, err error) {
	if in.Series == "" || in.No <= 0 || len(in.Lines) == 0 || in.IssueDate.IsZero() {
		return 0, fmt.Errorf("%w: series, number, issue date and lines are required", ErrProviderInvalid)
	}
	var gross int64
	for _, l := range in.Lines {
		if l.Quantity <= 0 || l.UnitPrice < 0 || !money.ValidBP(int64(l.VATRateBP)) {
			return 0, fmt.Errorf("%w: a line needs a positive quantity, a price and a VAT rate", ErrProviderInvalid)
		}
		net, err := money.Mul(l.UnitPrice, int64(l.Quantity))
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrProviderInvalid, err)
		}
		lineVAT, err := money.VAT(net, int64(l.VATRateBP))
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrProviderInvalid, err)
		}
		if gross, err = money.Sum(gross, net, lineVAT); err != nil {
			return 0, fmt.Errorf("%w: %w", ErrProviderInvalid, err)
		}
		if vat, err = money.Add(vat, lineVAT); err != nil {
			return 0, fmt.Errorf("%w: %w", ErrProviderInvalid, err)
		}
	}
	if gross != in.TotalGross {
		return 0, fmt.Errorf("%w: lines sum to %d, total is %d", ErrProviderInvalid, gross, in.TotalGross)
	}
	return vat, nil
}
