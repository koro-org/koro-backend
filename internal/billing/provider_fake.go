package billing

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"uuid"
)

// FakeProvider is an in-memory InvoiceProvider (BE-042): the tests' and
// the development adapter (BILLING_INVOICE_PROVIDER=mem; refused in
// staging and prod). It keeps the contract's idempotency rules and answers
// a job pending for PendingPolls polls before it is done; a document left
// to the provider (DocUnknown) is an e-Fatura for a party upserted with
// EInvoice, else an e-Arşiv. Safe for concurrent use.
type FakeProvider struct {
	// PendingPolls is how many PollJob calls answer pending before the job
	// is done (0: done at the first poll).
	PendingPolls int

	mu       sync.Mutex
	seq      int
	contacts map[string]string     // contact key → contact ID
	eInvoice map[string]bool       // contact ID → registered for e-Fatura
	drafts   map[string]*fakeDraft // "series/no" → draft
	byID     map[string]*fakeDraft // provider invoice ID → draft
	jobs     map[string]*fakeJob   // job ID → job
}

type fakeDraft struct {
	ref    ProviderRef
	in     DraftInvoice
	doc    DocumentType
	job    string
	issued bool
	no     string
	ettn   string
}

type fakeJob struct {
	draft *fakeDraft
	polls int
}

// NewFakeProvider returns an empty FakeProvider.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{contacts: map[string]string{}, eInvoice: map[string]bool{}, drafts: map[string]*fakeDraft{},
		byID: map[string]*fakeDraft{}, jobs: map[string]*fakeJob{}}
}

var _ InvoiceProvider = (*FakeProvider)(nil)

func (f *FakeProvider) next(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s-%06d", prefix, f.seq)
}

// UpsertContact implements InvoiceProvider: the tax number (else the
// title) keys the contact.
func (f *FakeProvider) UpsertContact(_ context.Context, p PartySnapshot) (string, error) {
	if strings.TrimSpace(p.Title) == "" {
		return "", fmt.Errorf("%w: contact title is required", ErrProviderInvalid)
	}
	key := "title:" + p.Title
	if p.TaxNo != "" {
		key = "vkn:" + p.TaxNo
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.contacts[key]; ok {
		f.eInvoice[id] = p.EInvoice
		return id, nil
	}
	id := f.next("contact")
	f.contacts[key], f.eInvoice[id] = id, p.EInvoice
	return id, nil
}

// CreateDraft implements InvoiceProvider (query before create).
func (f *FakeProvider) CreateDraft(_ context.Context, in DraftInvoice) (ProviderRef, error) {
	if _, err := checkDraft(in); err != nil {
		return ProviderRef{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	known := false
	for _, id := range f.contacts {
		known = known || id == in.ContactID
	}
	if !known {
		return ProviderRef{}, fmt.Errorf("%w: contact %q", ErrProviderNotFound, in.ContactID)
	}
	key := fmt.Sprintf("%s/%d", in.Series, in.No)
	if d, ok := f.drafts[key]; ok {
		return d.ref, nil
	}
	d := &fakeDraft{ref: ProviderRef{InvoiceID: f.next("invoice")}, in: in}
	f.drafts[key], f.byID[d.ref.InvoiceID] = d, d
	return d.ref, nil
}

// Issue implements InvoiceProvider: one job per draft.
func (f *FakeProvider) Issue(_ context.Context, ref ProviderRef, doc DocumentType) (string, error) {
	if doc != DocEFatura && doc != DocEArsiv && doc != DocUnknown {
		return "", fmt.Errorf("%w: document type %q", ErrProviderInvalid, doc)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.byID[ref.InvoiceID]
	if !ok {
		return "", fmt.Errorf("%w: invoice %q", ErrProviderNotFound, ref.InvoiceID)
	}
	if d.job != "" {
		return d.job, nil
	}
	if doc == DocUnknown {
		doc = DocEArsiv
		if f.eInvoice[d.in.ContactID] {
			doc = DocEFatura
		}
	}
	d.doc, d.job = doc, f.next("job")
	f.jobs[d.job] = &fakeJob{draft: d}
	return d.job, nil
}

// PollJob implements InvoiceProvider.
func (f *FakeProvider) PollJob(_ context.Context, jobID string) (JobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[jobID]
	if !ok {
		return JobStatus{}, fmt.Errorf("%w: job %q", ErrProviderNotFound, jobID)
	}
	if !j.draft.issued && j.polls < f.PendingPolls {
		j.polls++
		return JobStatus{State: JobPending, Ref: j.draft.ref}, nil
	}
	if !j.draft.issued {
		j.draft.issued = true
		j.draft.ref.EDocID = f.next("edoc")
		j.draft.no = fmt.Sprintf("%s%04d%09d", j.draft.in.Series, j.draft.in.IssueDate.Year, j.draft.in.No)
		j.draft.ettn = uuid.NewV7().String()
	}
	return JobStatus{State: JobDone, Ref: j.draft.ref, Doc: j.draft.doc, InvoiceNo: j.draft.no, ETTN: j.draft.ettn}, nil
}

// FetchPDF implements InvoiceProvider: a minimal PDF naming the number.
func (f *FakeProvider) FetchPDF(_ context.Context, ref ProviderRef) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.byID[ref.InvoiceID]
	if !ok {
		return nil, fmt.Errorf("%w: invoice %q", ErrProviderNotFound, ref.InvoiceID)
	}
	if !d.issued {
		return nil, ErrProviderNotReady
	}
	return []byte("%PDF-1.7\n% fake invoice " + d.no + "\n%%EOF\n"), nil
}
