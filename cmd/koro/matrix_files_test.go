package main

import (
	"context"
	"fmt"
	"time"

	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/platform/storage"
)

// matrixFilePDF is the object of fixture file_uploading and of the
// files.upload_object hook; bodies/files/complete.json holds its SHA-256
// and bodies/files/create_cv.json its size.
var matrixFilePDF = []byte("%PDF-1.7\n% koro authz matrix fixture\n%%EOF\n")

func (w *filesWiring) memObjects() (*storage.Mem, error) {
	mem, ok := w.objects.(*storage.Mem)
	if !ok {
		return nil, fmt.Errorf("files matrix needs storage.Mem (useMemStore), got %T", w.objects)
	}
	return mem, nil
}

// matrixFixtures seeds fixtures/files.yaml (03 §7.3) into the in-memory
// store and object storage installed by useMemStore.
func (w *filesWiring) matrixFixtures(fx *matrixFixtures) error {
	mem, err := w.memObjects()
	if err != nil {
		return err
	}
	ctx, now := context.Background(), time.Now().UTC()
	for _, f := range fx.Of("files") {
		if f.Kind != "file" {
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		by, err := fx.Ref(f, "uploaded_by")
		if err != nil {
			return err
		}
		purpose, _ := f.Fields["purpose"].(string)
		status, _ := f.Fields["status"].(string)
		size := int64(len(matrixFilePDF))
		row := files.File{
			ID: id, Purpose: files.Purpose(purpose), OwnerUserID: &by, UploadedBy: &by,
			Bucket: testBuckets.Quarantine, StorageKey: "matrix/" + f.Name, DeclaredMIME: files.MIMEPDF,
			SizeBytes: &size, Status: files.Status(status), ScanStatus: files.ScanPending,
			CreatedAt: now, UpdatedAt: now,
		}
		switch row.Status {
		case files.StatusUploading:
			mem.Upload(row.Bucket, row.StorageKey, matrixFilePDF, files.MIMEPDF)
		case files.StatusActive:
			row.SHA256, row.ScanStatus, row.Bucket = make([]byte, 32), files.ScanSkipped, testBuckets.Files
			mem.Upload(row.Bucket, row.StorageKey, matrixFilePDF, files.MIMEPDF)
		default:
			return fmt.Errorf("fixture %s: unknown status %q", f.Name, status)
		}
		if attached, _ := f.Fields["attached"].(bool); attached {
			module, entity := "students", authztest.ID(f.Name+"_owner")
			row.AttachedModule, row.AttachedEntityID, row.AttachedAt = &module, &entity, &now
		}
		if err := w.store.Insert(ctx, row); err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
	}
	return nil
}

// matrixHook runs the files scenario hooks:
//
//	files.upload_object {file: <name>}  the browser's PUT of matrixFilePDF
func (w *filesWiring) matrixHook(fx *matrixFixtures, name string, args map[string]any) error {
	if name != "upload_object" {
		return fmt.Errorf("unknown files hook %q", name)
	}
	mem, err := w.memObjects()
	if err != nil {
		return err
	}
	ref, _ := args["file"].(string)
	id, err := fx.ID(ref)
	if err != nil {
		return err
	}
	f, err := w.store.Get(context.Background(), id)
	if err != nil {
		return err
	}
	mem.Upload(f.Bucket, f.StorageKey, matrixFilePDF, f.DeclaredMIME)
	return nil
}
