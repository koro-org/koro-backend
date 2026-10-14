package billing

import (
	"context"

	"koro/internal/platform/civil"
)

// InvoiceProvider is the adapter contract of an e-invoice provider (01
// §4.21: Paraşüt, later Nilvera), used when a commerce policy's
// invoice_automation is 'provider'. The real Paraşüt client, its token
// rotation and the job polling are BE-053; FakeProvider implements the
// contract for the tests and as the skeleton of that path, and
// providerContract (provider_contract_test.go) is the suite every adapter
// passes (BE-053 runs it against the real adapter, in this package).
//
// Every call is idempotent from the caller's side (asynq retries them):
//
//   - UpsertContact keys the contact by the party's tax number: the same
//     party twice is the same contact ID.
//   - CreateDraft looks up (Series, No) first and creates only when the
//     provider has no draft with them ("sorgu-önce-oluştur").
//   - Issue on an already issued (or issuing) draft returns its job.
//
// Errors wrap ErrProviderNotFound (unknown reference or job),
// ErrProviderNotReady (FetchPDF before the document is issued) or
// ErrProviderInvalid (the provider rejected the request: not retried).
type InvoiceProvider interface {
	// UpsertContact creates or updates the party at the provider and
	// returns its contact ID (provider_contact_map).
	UpsertContact(ctx context.Context, p PartySnapshot) (contactID string, err error)
	// CreateDraft returns the provider's draft of in, creating it unless a
	// draft with in.Series and in.No exists.
	CreateDraft(ctx context.Context, in DraftInvoice) (ProviderRef, error)
	// Issue starts the official issue of the draft as doc (e-Fatura or
	// e-Arşiv) and returns the provider's trackable job ID.
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
	Ref       ProviderRef // EDocID set once done
	InvoiceNo string      // the official number once done
	Error     string      // the provider's message when failed (no personal data)
}
