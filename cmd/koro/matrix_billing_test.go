package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/billing"
	"koro/internal/files"
	"koro/internal/platform/civil"
)

// matrixFixtures seeds fixtures/billing.yaml (03 §7.3) into billing's
// in-memory store and files' store:
//
//	kind pdf      an active invoice_pdf upload (scan skipped: served while FILES_SCAN_REQUIRED=false) of
//	              uploaded_by, for org when given, attached to invoice when given
//	kind invoice  an invoice of kind (service_sale | commission) from issuer (a party fixture) of
//	              issuer_org (none: Koro's) to recipient of recipient_org, of mode, for order or payout;
//	              status planned, uploaded (uploaded_by, pdf), mismatch or issued (verified_by too)
//	kind payout_ref  only an ID (a payout of a commission invoice; billing keeps no payout row)
func (w *billingWiring) matrixFixtures(fx *matrixFixtures) error {
	all := fx.Of("billing")
	rank := map[string]int{"pdf": 0, "invoice": 1, "payout_ref": 2}
	for _, f := range all {
		if _, ok := rank[f.Kind]; !ok {
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	if w.late == nil {
		return errors.New("billing wiring was not built")
	}
	slices.SortStableFunc(all, func(a, b fixture) int { return cmp.Compare(rank[a.Kind], rank[b.Kind]) })
	ctx := context.Background()
	for _, f := range all {
		var err error
		switch f.Kind {
		case "pdf":
			err = w.matrixPDF(ctx, fx, f)
		case "invoice":
			err = w.matrixInvoice(ctx, fx, f)
		}
		if err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
	}
	return nil
}

// optRef resolves an optional fixture reference.
func optRef(fx *matrixFixtures, f fixture, field string) (*uuid.UUID, error) {
	if _, ok := f.Fields[field]; !ok {
		return nil, nil
	}
	id, err := fx.Ref(f, field)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func (w *billingWiring) matrixPDF(ctx context.Context, fx *matrixFixtures, f fixture) error {
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	by, err := fx.Ref(f, "uploaded_by")
	if err != nil {
		return err
	}
	org, err := optRef(fx, f, "org")
	if err != nil {
		return err
	}
	invoice, err := optRef(fx, f, "invoice")
	if err != nil {
		return err
	}
	objects, err := w.late.files.memObjects()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	size := int64(len(matrixFilePDF))
	row := files.File{
		ID: id, Purpose: files.PurposeInvoicePDF, OrgID: org, UploadedBy: &by, Bucket: testBuckets.Files,
		StorageKey: "matrix/" + f.Name, DeclaredMIME: files.MIMEPDF, SizeBytes: &size, SHA256: make([]byte, 32),
		Status: files.StatusActive, ScanStatus: files.ScanSkipped,
	}
	if scan, ok := f.Fields["scan"].(string); ok { // scan: infected, rejected after the upload (BE-154b)
		row.ScanStatus = files.ScanStatus(scan)
	}
	if invoice != nil {
		module := "billing"
		row.AttachedModule, row.AttachedEntityID, row.AttachedAt = &module, invoice, &now
	}
	objects.Upload(row.Bucket, row.StorageKey, matrixFilePDF, files.MIMEPDF)
	return w.late.files.store.Insert(ctx, row)
}

func (w *billingWiring) matrixInvoice(ctx context.Context, fx *matrixFixtures, f fixture) error {
	mem, ok := w.store.(*billing.MemStore)
	if !ok {
		return fmt.Errorf("the matrix needs billing's in-memory store, got %T", w.store)
	}
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	str := func(k string) string { s, _ := f.Fields[k].(string); return s }
	refs := map[string]*uuid.UUID{}
	for _, k := range []string{"issuer", "recipient", "issuer_org", "recipient_org", "order", "payout", "uploaded_by", "verified_by", "pdf"} {
		if refs[k], err = optRef(fx, f, k); err != nil {
			return err
		}
	}
	if refs["issuer"] == nil || refs["recipient"] == nil || refs["recipient_org"] == nil {
		return errors.New("issuer, recipient and recipient_org are required")
	}
	at := matrixOrgTime.Add(48 * time.Hour)
	inv := billing.Invoice{
		ID: id, OrderID: refs["order"], PayoutID: refs["payout"], Kind: billing.Kind(str("invoice_kind")),
		IssuerPartyID: *refs["issuer"], RecipientPartyID: *refs["recipient"], IssuerOrgID: refs["issuer_org"],
		RecipientOrgID: refs["recipient_org"], IssuerSnapshot: billing.Snapshot{Title: "Matris Düzenleyen"},
		RecipientSnapshot: billing.Snapshot{Title: "Matris Alıcı"}, IssuingMode: billing.IssuingMode(str("mode")),
		DocumentType: billing.DocUnknown, Provider: billing.ProviderNone, Status: billing.StatusPlanned,
		Trigger: billing.TriggerOnPayment, Currency: billing.Currency, NetAmount: 1_000_000, VATAmount: 200_000,
		TotalAmount: 1_200_000, PayableAmount: 1_200_000, IdempotencyKey: "matrix:" + f.Name,
	}
	if inv.Kind == billing.KindCommission {
		inv.Trigger = billing.TriggerOnPayout
		inv.NetAmount, inv.VATAmount, inv.TotalAmount, inv.PayableAmount = 100_000, 20_000, 120_000, 120_000
	}
	line := billing.Line{ID: authztest.ID(f.Name + "_line"), InvoiceID: id, Position: 1, Description: "Matris kalemi", Quantity: 1,
		UnitPriceAmount: inv.NetAmount, NetAmount: inv.NetAmount, VATRateBP: 2000, VATAmount: inv.VATAmount,
		TotalAmount: inv.TotalAmount}
	if _, _, err := mem.InsertInvoice(ctx, inv, []billing.Line{line}); err != nil {
		return err
	}
	status := str("status")
	if status == "" || status == string(billing.StatusPlanned) {
		return nil
	}
	if refs["uploaded_by"] == nil || refs["pdf"] == nil {
		return fmt.Errorf("status %s needs uploaded_by and pdf", status)
	}
	no, date := "MTR"+f.Name, civil.Date{Year: 2026, Month: time.October, Day: 2}
	next := inv
	next.DocumentType, next.InvoiceNo, next.IssueDate = billing.DocEArsiv, &no, &date
	next.PDFFileID, next.PDFSHA256, next.UploadedBy = refs["pdf"], make([]byte, 32), refs["uploaded_by"]
	switch status {
	case "uploaded":
	case string(billing.StatusIssued), string(billing.StatusMismatch):
		if refs["verified_by"] == nil {
			return fmt.Errorf("status %s needs verified_by", status)
		}
		verified := at.Add(time.Hour)
		next.Status, next.VerifiedBy, next.VerifiedAt = billing.Status(status), refs["verified_by"], &verified
		if status == string(billing.StatusIssued) {
			next.IssuedAt = &verified
		}
	default:
		return fmt.Errorf("status %q: want planned, uploaded, mismatch or issued", status)
	}
	_, err = mem.UpdateInvoice(ctx, next, billing.StatusPlanned)
	return err
}
