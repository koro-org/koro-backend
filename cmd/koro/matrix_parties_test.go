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
// parties store installed by useMemStore, and the party_document files
// into the files store and object storage of the container:
//
//	kind party     party_kind, legal_type, status, tax_no; org (a fixture org) when given;
//	               verified rows are verified by finance_admin_2; created and updated at
//	               matrixOrgTime, so the ETag of an untouched party is "1hmhuo3hji000"
//	kind payee     a payees row (org, party, status) relating the party to a seller org (k21)
//	kind file      an unattached, active party_document of uploaded_by for org
//	kind document  a legal_party_documents row of party uploaded by uploaded_by, with its
//	               attached file "<name>_file"
func (w *partiesWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	mem, ok := w.store.(*parties.MemStore)
	if !ok {
		return fmt.Errorf("parties matrix needs the in-memory store (useMemStore), got %T", w.store)
	}
	if w.late == nil {
		return errors.New("parties wiring was not built")
	}
	rank := map[string]int{"party": 0, "payee": 1, "file": 1, "document": 2}
	all := fx.Of("parties")
	slices.SortStableFunc(all, func(a, b fixture) int { return cmp.Compare(rank[a.Kind], rank[b.Kind]) })
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
				CreatedAt: t, UpdatedAt: t,
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
			org, err := fx.Ref(f, "org")
			if err != nil {
				return err
			}
			party, err := fx.Ref(f, "party")
			if err != nil {
				return err
			}
			mem.LinkPayee(id, org, party, str("status"))
		case "file":
			org, err := fx.Ref(f, "org")
			if err != nil {
				return err
			}
			by, err := fx.Ref(f, "uploaded_by")
			if err != nil {
				return err
			}
			if err := w.matrixFile(ctx, f.Name, id, by, &org, nil); err != nil {
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
			if err := w.matrixFile(ctx, f.Name+"_file", fileID, by, nil, &id); err != nil {
				return err
			}
			if _, err := mem.InsertDocument(ctx, parties.Document{ID: id, LegalPartyID: party, Kind: parties.DocTaxCertificate,
				FileID: fileID, UploadedBy: by, CreatedAt: matrixOrgTime}); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}

// matrixFile seeds an active party_document file uploaded by by (for org)
// into the files store and object storage: skipped by the no-op scanner,
// so it is served while FILES_SCAN_REQUIRED=false; attached to the
// document when given.
func (w *partiesWiring) matrixFile(ctx context.Context, name string, id, by uuid.UUID, org, document *uuid.UUID) error {
	objects, err := w.late.files.memObjects()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	size := int64(len(matrixFilePDF))
	row := files.File{
		ID: id, Purpose: files.PurposePartyDocument, OrgID: org, UploadedBy: &by, Bucket: testBuckets.Files,
		StorageKey: "matrix/" + name, DeclaredMIME: files.MIMEPDF, SizeBytes: &size, SHA256: make([]byte, 32),
		Status: files.StatusActive, ScanStatus: files.ScanSkipped, CreatedAt: now, UpdatedAt: now,
	}
	if document != nil {
		module := "parties"
		row.AttachedModule, row.AttachedEntityID, row.AttachedAt = &module, document, &now
	}
	objects.Upload(row.Bucket, row.StorageKey, matrixFilePDF, files.MIMEPDF)
	if err := w.late.files.store.Insert(ctx, row); err != nil {
		return fmt.Errorf("fixture %s: %w", name, err)
	}
	return nil
}
