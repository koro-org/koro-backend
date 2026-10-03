package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/kvkk"
)

// matrixDSRReceivedAt is the receipt time of every kvkk fixture; the
// in-memory store stamps it as updated_at, so the ETag of an untouched
// fixture is "DDNFR4TI7YAAA" (testdata/authz/kvkk.yaml If-Match).
var matrixDSRReceivedAt = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

// matrixFixtures seeds fixtures/kvkk.yaml (03 §7.3) into the in-memory
// store installed by useMemStore; export_file creates a real export file in
// the in-memory files store and attaches it.
func (w *kvkkWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	for _, f := range fx.Of("kvkk") {
		switch f.Kind {
		case "breach", "subprocessor":
			if err := w.matrixRecord(fx, f); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
			continue
		case "dsr":
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		owner, err := fx.Ref(f, "owner")
		if err != nil {
			return err
		}
		typ, _ := f.Fields["type"].(string)
		status, _ := f.Fields["status"].(string)
		d := kvkk.DSR{ID: id, UserID: &owner, Type: kvkk.Type(typ), Channel: kvkk.ChannelInApp, Status: kvkk.StatusReceived,
			ReceivedAt: matrixDSRReceivedAt, DueAt: matrixDSRReceivedAt.Add(kvkk.LegalPeriod)}
		if v, _ := f.Fields["verified"].(bool); v {
			at := matrixDSRReceivedAt
			d.IdentityVerifiedAt = &at
		}
		if _, ok := f.Fields["first_approver"]; ok {
			first, err := fx.Ref(f, "first_approver")
			if err != nil {
				return err
			}
			d.FirstApprover = &first
		}
		d.Status = kvkk.Status(status)
		if d.Status == kvkk.StatusCompleted {
			done := matrixDSRReceivedAt.Add(time.Hour)
			d.CompletedAt, d.ResponseSummary = &done, "Matris fikstürü"
		}
		if err := w.store.Insert(ctx, d); err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
		if v, _ := f.Fields["export_file"].(bool); v {
			if err := w.matrixExportFile(ctx, d); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		}
	}
	return nil
}

// matrixExportFile stores a small export zip for d and attaches it. The
// store keeps updated_at = received_at only for untouched rows, so the
// export fixture is not used with If-Match.
func (w *kvkkWiring) matrixExportFile(ctx context.Context, d kvkk.DSR) error {
	fileID, err := w.files.PutGenerated(ctx, files.GeneratedFile{Purpose: files.PurposeDSRExport, OwnerUserID: d.UserID,
		Name: "koro-kisisel-veriler.zip", ContentType: files.MIMEZIP, Content: []byte("PK\x05\x06" + string(make([]byte, 18)))})
	if err != nil {
		return err
	}
	if err := w.files.Attach(ctx, authz.SystemActor("kvkk.matrix"), fileID,
		files.Owner{Module: "kvkk", EntityID: d.ID, Purpose: files.PurposeDSRExport}); err != nil {
		return err
	}
	d.ExportFileID = &fileID
	return w.store.Update(ctx, d)
}

// matrixRecord seeds a breach or a sub-processor of fixtures/kvkk.yaml with
// the fixed timestamps of matrixRecordsAt (ETag "DDNFR4TI7YAAA").
func (w *kvkkWiring) matrixRecord(fx *matrixFixtures, f fixture) error {
	mem, ok := w.records.(*kvkk.MemRecords)
	if !ok {
		return errors.New("kvkk records are not in memory")
	}
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	switch f.Kind {
	case "breach":
		owner, err := fx.Ref(f, "owner")
		if err != nil {
			return err
		}
		status, _ := f.Fields["status"].(string)
		detected := matrixRecordsAt.Add(-time.Hour)
		b := kvkk.Breach{ID: id, Title: "Matris ihlali", DetectedAt: detected, AuthorityDeadlineAt: detected.Add(kvkk.AuthorityPeriod),
			DataCategories: []string{"contact"}, Description: "Yanlış alıcıya gönderilen toplu e-posta.",
			Status: kvkk.BreachStatus(status), OwnerUserID: owner, CreatedAt: matrixRecordsAt, UpdatedAt: matrixRecordsAt}
		if b.Status == kvkk.BreachClosed {
			at := matrixRecordsAt
			b.ClosedAt = &at
		}
		mem.PutBreach(b)
	case "subprocessor":
		name, _ := f.Fields["name"].(string)
		mem.PutSubprocessor(kvkk.Subprocessor{ID: id, Name: name, Service: "E-posta gönderimi", DataCategories: []string{"contact"},
			Region: "eu-central-1", CountryCode: "DE", TransferMechanism: "standard_contract", Active: true,
			CreatedAt: matrixRecordsAt, UpdatedAt: matrixRecordsAt})
	}
	return nil
}
