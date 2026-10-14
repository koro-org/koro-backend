package billing_test

import (
	"errors"
	"testing"

	"koro/internal/billing"
	"koro/internal/platform/civil"
)

// TestFakeProvider_ContractSuite runs the InvoiceProvider contract suite
// (provider_contract_test.go) against FakeProvider; BE-053 runs the same
// suite against the Paraşüt adapter. The jobs stay pending for two polls.
func TestFakeProvider_ContractSuite(t *testing.T) { //nolint:tparallel // the contract's steps share the provider's state and run in order
	t.Parallel()
	p := billing.NewFakeProvider()
	p.PendingPolls = 2
	date, err := civil.Parse("2026-10-14")
	if err != nil {
		t.Fatal(err)
	}
	providerContract(t, p, providerFixture{
		Series: "KRT", FirstNo: 1, IssueDate: date, MaxPolls: 5,
		Contact: billing.PartySnapshot{Snapshot: billing.Snapshot{Title: "Sözleşme Test A.Ş.", TaxNo: "1000000001"}, LegalType: "company"},
		Other:   billing.PartySnapshot{Snapshot: billing.Snapshot{Title: "Sözleşme Test Vakfı", TaxNo: "1000000002"}, LegalType: "foundation"},
	})
}

// TestFakeProvider_StaysPendingWithinPolls: a job answers pending for
// PendingPolls polls (the worker retries, BE-053's billing.poll_jobs).
func TestFakeProvider_StaysPendingWithinPolls(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p := billing.NewFakeProvider()
	p.PendingPolls = 1
	c, err := p.UpsertContact(ctx, billing.PartySnapshot{Snapshot: billing.Snapshot{Title: "Bekleme Test A.Ş."}})
	if err != nil {
		t.Fatal(err)
	}
	date, _ := civil.Parse("2026-10-14")
	ref, err := p.CreateDraft(ctx, billing.DraftInvoice{ContactID: c, Series: "KRP", No: 1, IssueDate: date, TotalGross: 120,
		Lines: []billing.DraftLine{{Description: "x", Quantity: 1, UnitPrice: 100, VATRateBP: 2000}}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := p.Issue(ctx, ref, billing.DocEFatura)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := p.PollJob(ctx, job); err != nil || st.State != billing.JobPending {
		t.Fatalf("first poll = %+v, %v", st, err)
	}
	if _, err := p.FetchPDF(ctx, ref); !errors.Is(err, billing.ErrProviderNotReady) {
		t.Fatalf("FetchPDF while pending = %v", err)
	}
	if st, err := p.PollJob(ctx, job); err != nil || st.State != billing.JobDone {
		t.Fatalf("second poll = %+v, %v", st, err)
	}
}
