package files_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/files"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// storeContract is the contract both stores pass (05 §13.3): the in-memory
// one here, the GORM one against PostgreSQL in store_integration_test.go.
func storeContract(t *testing.T, newStore func(t *testing.T) (files.Store, db.TxManager)) {
	t.Helper()
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	user := uuid.NewV7()
	row := func(purpose files.Purpose, created time.Time) files.File {
		id := uuid.NewV7()
		size := int64(42)
		return files.File{
			ID: id, Purpose: purpose, OwnerUserID: &user, UploadedBy: &user, Bucket: "q", StorageKey: string(purpose) + "/" + id.String(),
			DeclaredMIME: files.MIMEPDF, SizeBytes: &size, Status: files.StatusUploading, ScanStatus: files.ScanPending,
			CreatedAt: created, UpdatedAt: created,
		}
	}
	activate := func(f files.File) files.File {
		mime := files.MIMEPDF
		f.Status, f.SHA256, f.DetectedMIME = files.StatusActive, bytes.Repeat([]byte{7}, 32), &mime
		return f
	}

	t.Run("insert get update delete", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		ctx := t.Context()
		f := row(files.PurposeCV, base)
		name := "CV.pdf"
		f.OriginalName = &name
		if err := s.Insert(ctx, f); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		got, err := s.Get(ctx, f.ID)
		if err != nil || got.Purpose != f.Purpose || *got.OriginalName != name || *got.UploadedBy != user || *got.SizeBytes != 42 ||
			got.Status != files.StatusUploading || got.SHA256 != nil || !got.CreatedAt.Equal(base) {
			t.Fatalf("Get = %+v, %v", got, err)
		}
		got = activate(got)
		module, entity, now := "students", uuid.NewV7(), base.Add(time.Hour)
		got.AttachedModule, got.AttachedEntityID, got.AttachedAt = &module, &entity, &now
		if err := s.Update(ctx, got); err != nil {
			t.Fatalf("Update: %v", err)
		}
		again, _ := s.Get(ctx, f.ID)
		if again.Status != files.StatusActive || !bytes.Equal(again.SHA256, got.SHA256) || !again.Attached() ||
			*again.AttachedEntityID != entity || !again.CreatedAt.Equal(base) {
			t.Fatalf("after Update: %+v", again)
		}
		if err := s.Delete(ctx, f.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, f.ID); !errors.Is(err, files.ErrNotFound) {
			t.Fatalf("Get after Delete = %v", err)
		}
		if err := s.Delete(ctx, f.ID); err != nil {
			t.Fatalf("Delete of a missing row = %v", err)
		}
		if err := s.Update(ctx, got); !errors.Is(err, files.ErrNotFound) {
			t.Fatalf("Update of a missing row = %v", err)
		}
	})

	t.Run("constraints", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		ctx := t.Context()
		f := row(files.PurposeCV, base)
		if err := s.Insert(ctx, f); err != nil {
			t.Fatal(err)
		}
		dup := row(files.PurposeCV, base)
		dup.StorageKey = f.StorageKey
		if err := s.Insert(ctx, dup); !errors.Is(err, problem.ErrConflict) {
			t.Fatalf("duplicate storage key = %v, want 409 (files_storage_key_uq)", err)
		}
		bad := row(files.PurposeCV, base)
		bad.Status = files.StatusActive // without sha256: files_active_chk
		if err := s.Insert(ctx, bad); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("active without sha256 = %v, want 422", err)
		}
		half := activate(row(files.PurposeCV, base))
		module := "students"
		half.AttachedModule = &module // without entity: files_attached_chk
		if err := s.Insert(ctx, half); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("half attached = %v, want 422", err)
		}
		unknown := row("selfie", base)
		if err := s.Insert(ctx, unknown); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("unknown purpose = %v, want 422", err)
		}
	})

	t.Run("orphans and pending scans page by id", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		ctx := t.Context()
		var old []uuid.UUID
		for range 3 {
			f := row(files.PurposeCV, base)
			if err := s.Insert(ctx, f); err != nil {
				t.Fatal(err)
			}
			old = append(old, f.ID)
		}
		young := row(files.PurposeCV, base.Add(48*time.Hour))
		attached := activate(row(files.PurposeCV, base))
		module, entity := "students", uuid.NewV7()
		attached.AttachedModule, attached.AttachedEntityID, attached.AttachedAt = &module, &entity, &base
		attached.ScanStatus = files.ScanSkipped
		pending := activate(row(files.PurposeAvatar, base))
		pending.DeclaredMIME = files.MIMEPNG
		for _, f := range []files.File{young, attached, pending} {
			if err := s.Insert(ctx, f); err != nil {
				t.Fatal(err)
			}
		}
		old = append(old, pending.ID)
		cutoff := base.Add(24 * time.Hour)
		first, err := s.Orphans(ctx, cutoff, uuid.Nil(), 2)
		if err != nil || len(first) != 2 {
			t.Fatalf("Orphans page 1 = %d, %v", len(first), err)
		}
		rest, _ := s.Orphans(ctx, cutoff, first[1].ID, 10)
		var ids []uuid.UUID
		for _, f := range append(first, rest...) {
			ids = append(ids, f.ID)
		}
		if len(ids) != 4 {
			t.Fatalf("orphans = %v, want the 4 old unattached rows %v", ids, old)
		}
		for i := range ids[1:] {
			if bytes.Compare(ids[i][:], ids[i+1][:]) >= 0 {
				t.Fatalf("orphans not in id order: %v", ids)
			}
		}
		scans, err := s.PendingScans(ctx, cutoff, uuid.Nil(), 10)
		if err != nil || len(scans) != 1 || scans[0].ID != pending.ID {
			t.Fatalf("PendingScans = %+v, %v", scans, err)
		}
		mine, err := s.UnattachedOf(ctx, user)
		if err != nil || len(mine) != 5 {
			t.Fatalf("UnattachedOf = %d rows, %v; want 5", len(mine), err)
		}
	})

	t.Run("lock in a transaction", func(t *testing.T) {
		t.Parallel()
		s, tx := newStore(t)
		f := row(files.PurposeCV, base)
		if err := s.Insert(t.Context(), f); err != nil {
			t.Fatal(err)
		}
		err := tx.WithTx(t.Context(), func(ctx context.Context) error {
			got, err := s.Lock(ctx, f.ID)
			if err != nil || got.ID != f.ID {
				t.Fatalf("Lock = %v", err)
			}
			_, err = s.Lock(ctx, uuid.NewV7())
			if !errors.Is(err, files.ErrNotFound) {
				t.Fatalf("Lock(missing) = %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(*testing.T) (files.Store, db.TxManager) {
		return files.NewMemStore(func() time.Time { return time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC) }), db.NopTx{}
	})
}
