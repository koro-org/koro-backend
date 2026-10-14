package billing_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"koro/internal/billing"
	"koro/internal/platform/civil"
)

// providerFixture is what the InvoiceProvider contract suite needs from the adapter's environment.
type providerFixture struct {
	// Series is a reserved invoice series of this run and FirstNo the first
	// number the suite may use (it uses FirstNo and FirstNo+1): a provider
	// keeps its drafts, so a rerun needs fresh numbers.
	Series  string
	FirstNo int
	// Contact and Other are two parties with different tax numbers.
	Contact, Other billing.PartySnapshot
	// IssueDate is the issue date of the drafts.
	IssueDate civil.Date
	// MaxPolls is how many PollJob calls a job may take to finish and
	// PollWait the pause between them (0 for the fake).
	MaxPolls int
	PollWait time.Duration
}

// providerContract is the contract suite of billing.InvoiceProvider
// (BE-042): every adapter passes it — FakeProvider in
// TestFakeProvider_ContractSuite, the Paraşüt adapter of BE-053 against its
// sandbox (a test of this package). Its steps share the provider's state
// and run in order.
func providerContract(t *testing.T, p billing.InvoiceProvider, fx providerFixture) {
	t.Helper()
	ctx := t.Context()
	if fx.MaxPolls <= 0 {
		fx.MaxPolls = 5
	}

	var contact string
	t.Run("contact is upserted by tax number", func(t *testing.T) {
		id, err := p.UpsertContact(ctx, fx.Contact)
		if err != nil || id == "" {
			t.Fatalf("UpsertContact = %q, %v", id, err)
		}
		again, err := p.UpsertContact(ctx, fx.Contact)
		if err != nil || again != id {
			t.Fatalf("UpsertContact again = %q, %v; want the same contact %q", again, err, id)
		}
		other, err := p.UpsertContact(ctx, fx.Other)
		if err != nil || other == id {
			t.Fatalf("UpsertContact(other party) = %q, %v; want another contact", other, err)
		}
		if _, err := p.UpsertContact(ctx, billing.PartySnapshot{}); !errors.Is(err, billing.ErrProviderInvalid) {
			t.Fatalf("UpsertContact(no title) = %v, want ErrProviderInvalid", err)
		}
		contact = id
	})
	if contact == "" {
		t.Fatal("no contact: the remaining steps need one")
	}

	draft := billing.DraftInvoice{
		InvoiceID: "koro-contract-" + fx.Series, ContactID: contact, Series: fx.Series, No: fx.FirstNo,
		IssueDate: fx.IssueDate, Currency: billing.Currency, TotalGross: 1_200_000,
		Lines: []billing.DraftLine{{Description: "Contract suite service", Quantity: 1, UnitPrice: 1_000_000, VATRateBP: 2000}},
	}
	var ref billing.ProviderRef
	t.Run("draft is created once per series and number", func(t *testing.T) {
		r, err := p.CreateDraft(ctx, draft)
		if err != nil || r.InvoiceID == "" {
			t.Fatalf("CreateDraft = %+v, %v", r, err)
		}
		again, err := p.CreateDraft(ctx, draft)
		if err != nil || again.InvoiceID != r.InvoiceID {
			t.Fatalf("CreateDraft again = %+v, %v; want the first draft %q", again, err, r.InvoiceID)
		}
		next := draft
		next.No = fx.FirstNo + 1
		other, err := p.CreateDraft(ctx, next)
		if err != nil || other.InvoiceID == r.InvoiceID {
			t.Fatalf("CreateDraft(next number) = %+v, %v; want another draft", other, err)
		}
		bad := draft
		bad.No, bad.TotalGross = fx.FirstNo+2, 1
		if _, err := p.CreateDraft(ctx, bad); !errors.Is(err, billing.ErrProviderInvalid) {
			t.Fatalf("CreateDraft(wrong total) = %v, want ErrProviderInvalid", err)
		}
		unknown := draft
		unknown.No, unknown.ContactID = fx.FirstNo+3, "no-such-contact"
		if _, err := p.CreateDraft(ctx, unknown); !errors.Is(err, billing.ErrProviderNotFound) &&
			!errors.Is(err, billing.ErrProviderInvalid) {
			t.Fatalf("CreateDraft(unknown contact) = %v, want ErrProviderNotFound or ErrProviderInvalid", err)
		}
		ref = r
	})
	if ref.InvoiceID == "" {
		t.Fatal("no draft: the remaining steps need one")
	}

	t.Run("no PDF before the document is issued", func(t *testing.T) {
		if _, err := p.FetchPDF(ctx, ref); !errors.Is(err, billing.ErrProviderNotReady) {
			t.Fatalf("FetchPDF(draft) = %v, want ErrProviderNotReady", err)
		}
		if _, err := p.FetchPDF(ctx, billing.ProviderRef{InvoiceID: "no-such-invoice"}); !errors.Is(err, billing.ErrProviderNotFound) {
			t.Fatalf("FetchPDF(unknown) = %v, want ErrProviderNotFound", err)
		}
	})

	var job string
	t.Run("issue returns one job per draft", func(t *testing.T) {
		j, err := p.Issue(ctx, ref, billing.DocEArsiv)
		if err != nil || j == "" {
			t.Fatalf("Issue = %q, %v", j, err)
		}
		again, err := p.Issue(ctx, ref, billing.DocEArsiv)
		if err != nil || again != j {
			t.Fatalf("Issue again = %q, %v; want the first job %q", again, err, j)
		}
		if _, err := p.Issue(ctx, billing.ProviderRef{InvoiceID: "no-such-invoice"}, billing.DocEArsiv); !errors.Is(err, billing.ErrProviderNotFound) {
			t.Fatalf("Issue(unknown) = %v, want ErrProviderNotFound", err)
		}
		if _, err := p.Issue(ctx, ref, billing.DocPaper); !errors.Is(err, billing.ErrProviderInvalid) {
			t.Fatalf("Issue(paper) = %v, want ErrProviderInvalid", err)
		}
		job = j
	})
	if job == "" {
		t.Fatal("no job: the remaining steps need one")
	}

	t.Run("job finishes with the official number and the PDF", func(t *testing.T) {
		var st billing.JobStatus
		for i := range fx.MaxPolls {
			var err error
			if st, err = p.PollJob(ctx, job); err != nil {
				t.Fatalf("PollJob #%d: %v", i+1, err)
			}
			if st.State != billing.JobPending {
				break
			}
			if fx.PollWait > 0 {
				time.Sleep(fx.PollWait)
			}
		}
		if st.State != billing.JobDone || st.InvoiceNo == "" || st.Ref.EDocID == "" || st.Ref.InvoiceID != ref.InvoiceID {
			t.Fatalf("job after %d polls = %+v; want done with a number and an e-document", fx.MaxPolls, st)
		}
		again, err := p.PollJob(ctx, job)
		if err != nil || again.State != billing.JobDone || again.InvoiceNo != st.InvoiceNo {
			t.Fatalf("PollJob(done) again = %+v, %v", again, err)
		}
		if _, err := p.PollJob(ctx, "no-such-job"); !errors.Is(err, billing.ErrProviderNotFound) {
			t.Fatalf("PollJob(unknown) = %v, want ErrProviderNotFound", err)
		}
		pdf, err := p.FetchPDF(ctx, st.Ref)
		if err != nil || !bytes.HasPrefix(pdf, []byte("%PDF-")) {
			t.Fatalf("FetchPDF = %d bytes, %v; want a PDF", len(pdf), err)
		}
	})
}
