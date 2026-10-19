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
	"koro/internal/files"
	"koro/internal/parties"
)

// matrixFixtures seeds fixtures/parties.yaml (03 §7.3) into the in-memory
// parties store installed by useMemStore, and the party_document and
// bank_account_evidence files into the files store and object storage of
// the container:
//
//	kind party     party_kind, legal_type, status, tax_no; org (a fixture org) when given;
//	               verified rows are verified by finance_admin_2; created and updated at
//	               matrixOrgTime (the store's clock is pinned there while seeding, SeedAt),
//	               so the ETag of an untouched party is "1hmhuo3hji000"
//	kind payee     a payees row (org, party, status, is_default) created by platform_admin_2 at
//	               matrixOrgTime (ETag "1hmhuo3hji000"); an active one verified by finance_admin_2
//	kind file      an unattached, active file of uploaded_by for org (purpose party_document, or
//	               the purpose field: bank_account_evidence)
//	kind document  a legal_party_documents row of party uploaded by uploaded_by, with its
//	               attached file "<name>_file"
//	kind account   a payee_bank_accounts row of payee (iban, status, submitted_by, admin: an
//	               admin submission, verified_by, approved_by); the holder is the payee party's
//	               title; its evidence "<name>_file" is attached
func (w *partiesWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	mem, ok := w.store.(*parties.MemStore)
	if !ok {
		return fmt.Errorf("parties matrix needs the in-memory store (useMemStore), got %T", w.store)
	}
	if w.late == nil {
		return errors.New("parties wiring was not built")
	}
	rank := map[string]int{"party": 0, "payee": 1, "file": 1, "document": 2, "account": 2}
	all := fx.Of("parties")
	slices.SortStableFunc(all, func(a, b fixture) int { return cmp.Compare(rank[a.Kind], rank[b.Kind]) })
	return mem.SeedAt(matrixOrgTime, func() error { return w.seedFixtures(ctx, fx, mem, all) })
}

// seedFixtures seeds the sorted parties fixtures (matrixFixtures).
func (w *partiesWiring) seedFixtures(ctx context.Context, fx *matrixFixtures, mem *parties.MemStore, all []fixture) error {
	verifier := authztest.ID("finance_admin_2")
	for _, f := range all {
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		str := func(k string) string { s, _ := f.Fields[k].(string); return s }
		switch f.Kind {
		case "party":
			t := matrixOrgTime
			p := parties.LegalParty{
				ID: id, Kind: parties.Kind(str("party_kind")), LegalType: parties.LegalType(str("legal_type")),
				Title: "Matris " + f.Name, VATStatus: parties.VATUnknown, Status: parties.Status(str("status")),
			}
			if p.Kind != parties.KindPlatform {
				office, email := "Matris Vergi Dairesi", f.Name+"@matrix.koro.test"
				p.TaxOffice, p.Email = &office, &email
				p.Address = parties.Address{Line1: "Matris Cad. 1", City: "İstanbul", Country: "TR"}
			}
			if v := str("tax_no"); v != "" {
				p.TaxNo = &v
			}
			if _, ok := f.Fields["org"]; ok {
				org, err := fx.Ref(f, "org")
				if err != nil {
					return err
				}
				p.OrgID = &org
			}
			if p.Status == parties.StatusVerified {
				p.VerifiedAt, p.VerifiedBy = &t, &verifier
			}
			if _, err := mem.InsertParty(ctx, p); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "payee":
			if err := w.matrixPayee(ctx, fx, mem, f, id); err != nil {
				return err
			}
		case "file":
			org, err := fx.Ref(f, "org")
			if err != nil {
				return err
			}
			by, err := fx.Ref(f, "uploaded_by")
			if err != nil {
				return err
			}
			purpose := files.PurposePartyDocument
			if p := str("purpose"); p != "" {
				purpose = files.Purpose(p)
			}
			if err := w.matrixFile(ctx, f.Name, id, purpose, by, &org, nil); err != nil {
				return err
			}
		case "document":
			party, err := fx.Ref(f, "party")
			if err != nil {
				return err
			}
			by, err := fx.Ref(f, "uploaded_by")
			if err != nil {
				return err
			}
			fileID := authztest.ID(f.Name + "_file")
			if err := w.matrixFile(ctx, f.Name+"_file", fileID, files.PurposePartyDocument, by, nil, &id); err != nil {
				return err
			}
			if _, err := mem.InsertDocument(ctx, parties.Document{ID: id, LegalPartyID: party, Kind: parties.DocTaxCertificate,
				FileID: fileID, UploadedBy: by}); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "account":
			if err := w.matrixAccount(ctx, fx, mem, f, id); err != nil {
				return err
			}
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}

// matrixPayee seeds a payees row at matrixOrgTime.
func (w *partiesWiring) matrixPayee(ctx context.Context, fx *matrixFixtures, mem *parties.MemStore, f fixture, id uuid.UUID) error {
	org, err := fx.Ref(f, "org")
	if err != nil {
		return err
	}
	party, err := fx.Ref(f, "party")
	if err != nil {
		return err
	}
	status, _ := f.Fields["status"].(string)
	def, _ := f.Fields["is_default"].(bool)
	t, verifier := matrixOrgTime, authztest.ID("finance_admin_2")
	p := parties.Payee{
		ID: id, LegalPartyID: party, OrgID: org, Status: parties.PayeeStatus(status), IsDefault: def,
		PayoutMethod: parties.PayoutBankEFT, InvoicingCapability: parties.InvoicingUnknown, ValidFrom: t,
		CreatedBy: authztest.ID("platform_admin_2"),
	}
	if p.Status == parties.PayeeActive {
		p.VerifiedAt, p.VerifiedBy = &t, &verifier
	}
	if _, err := mem.InsertPayee(ctx, p); err != nil {
		return fmt.Errorf("fixture %s: %w", f.Name, err)
	}
	return nil
}

// matrixAccount seeds a bank account through its real states: inserted
// pending, then verified, approved as the fixture says (the in-memory
// store applies the CHECKs of 000097 on every step).
func (w *partiesWiring) matrixAccount(ctx context.Context, fx *matrixFixtures, mem *parties.MemStore, f fixture, id uuid.UUID) error {
	payeeID, err := fx.Ref(f, "payee")
	if err != nil {
		return err
	}
	by, err := fx.Ref(f, "submitted_by")
	if err != nil {
		return err
	}
	py, err := mem.GetPayee(ctx, payeeID, false)
	if err != nil {
		return fmt.Errorf("fixture %s: %w", f.Name, err)
	}
	party, err := mem.GetParty(ctx, py.LegalPartyID)
	if err != nil {
		return fmt.Errorf("fixture %s: %w", f.Name, err)
	}
	fileID := authztest.ID(f.Name + "_file")
	org := py.OrgID
	if err := w.matrixFile(ctx, f.Name+"_file", fileID, files.PurposeBankAccountEvidence, by, &org, &id); err != nil {
		return err
	}
	admin, _ := f.Fields["admin"].(bool)
	ibanNo, _ := f.Fields["iban"].(string)
	want, _ := f.Fields["status"].(string)
	t := matrixOrgTime
	b := parties.BankAccount{
		ID: id, PayeeID: py.ID, OrgID: py.OrgID, IBAN: ibanNo, HolderName: party.Title, EvidenceFileID: fileID,
		Status: parties.AccountPendingVerification, SubmittedBy: by, SubmittedByAdmin: admin, SubmittedAt: t,
	}
	if admin {
		b.Status = parties.AccountPendingApproval
	}
	if b, err = mem.InsertBankAccount(ctx, b); err != nil {
		return fmt.Errorf("fixture %s: %w", f.Name, err)
	}
	step := func(next parties.BankAccount) error {
		from := b.Status
		var err error
		if b, err = mem.TransitionBankAccount(ctx, next, from); err != nil {
			return fmt.Errorf("fixture %s (%s → %s): %w", f.Name, from, next.Status, err)
		}
		return nil
	}
	if _, ok := f.Fields["verified_by"]; ok {
		v, err := fx.Ref(f, "verified_by")
		if err != nil {
			return err
		}
		next := b
		next.Status, next.VerifiedBy, next.VerifiedAt = parties.AccountPendingApproval, &v, &t
		if err := step(next); err != nil {
			return err
		}
	}
	if _, ok := f.Fields["approved_by"]; ok {
		a, err := fx.Ref(f, "approved_by")
		if err != nil {
			return err
		}
		next := b
		next.Status, next.ApprovedBy, next.ApprovedAt = parties.AccountApproved, &a, &t
		if err := step(next); err != nil {
			return err
		}
	}
	if string(b.Status) != want {
		return fmt.Errorf("fixture %s: status %s, the fixture says %s", f.Name, b.Status, want)
	}
	return nil
}

// matrixFile seeds an active file of purpose uploaded by by (for org) into
// the files store and object storage: skipped by the no-op scanner, so it
// is served while FILES_SCAN_REQUIRED=false; attached to its document or
// bank account (owner) when given.
func (w *partiesWiring) matrixFile(ctx context.Context, name string, id uuid.UUID, purpose files.Purpose, by uuid.UUID, org, owner *uuid.UUID) error {
	objects, err := w.late.files.memObjects()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	size := int64(len(matrixFilePDF))
	row := files.File{
		ID: id, Purpose: purpose, OrgID: org, UploadedBy: &by, Bucket: testBuckets.Files,
		StorageKey: "matrix/" + name, DeclaredMIME: files.MIMEPDF, SizeBytes: &size, SHA256: make([]byte, 32),
		Status: files.StatusActive, ScanStatus: files.ScanSkipped, CreatedAt: now, UpdatedAt: now,
	}
	if owner != nil {
		module := "parties"
		row.AttachedModule, row.AttachedEntityID, row.AttachedAt = &module, owner, &now
	}
	objects.Upload(row.Bucket, row.StorageKey, matrixFilePDF, files.MIMEPDF)
	if err := w.late.files.store.Insert(ctx, row); err != nil {
		return fmt.Errorf("fixture %s: %w", name, err)
	}
	return nil
}
