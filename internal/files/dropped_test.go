package files_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/problem"
)

// deleter records Delete calls and fails on fail.
type deleter struct {
	deleted []uuid.UUID
	fail    uuid.UUID
}

var errDelete = errors.New("delete failed")

func (d *deleter) Delete(_ context.Context, _ authz.Actor, id uuid.UUID) error {
	if id == d.fail {
		return errDelete
	}
	d.deleted = append(d.deleted, id)
	return nil
}

// TestDeleteDropped (BE-155): only the files of before that after no longer
// lists are deleted, in order; nothing dropped needs no Files; a nil Files
// with something to delete is 503 (the write is refused, the file is not
// left behind); a Delete error stops the caller's transaction.
func TestDeleteDropped(t *testing.T) {
	t.Parallel()
	ctx, a := t.Context(), authz.SystemActor("test")
	x, y, z := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()

	d := &deleter{}
	if err := files.DeleteDropped(ctx, d, a, []uuid.UUID{x, y, z}, []uuid.UUID{z, uuid.NewV7()}); err != nil ||
		!slices.Equal(d.deleted, []uuid.UUID{x, y}) {
		t.Fatalf("DeleteDropped = %v, deleted %v", err, d.deleted)
	}
	d = &deleter{}
	if err := files.DeleteReplaced(ctx, d, a, &x, &x); err != nil || len(d.deleted) != 0 {
		t.Fatalf("same file: %v, deleted %v", err, d.deleted)
	}
	if err := files.DeleteReplaced(ctx, d, a, nil, &x); err != nil || len(d.deleted) != 0 {
		t.Fatalf("no old file: %v, deleted %v", err, d.deleted)
	}
	if err := files.DeleteReplaced(ctx, d, a, &x, &y); err != nil || !slices.Equal(d.deleted, []uuid.UUID{x}) {
		t.Fatalf("replaced: %v, deleted %v", err, d.deleted)
	}
	if err := files.DeleteReplaced(ctx, d, a, &y, nil); err != nil || !slices.Equal(d.deleted, []uuid.UUID{x, y}) {
		t.Fatalf("cleared: %v, deleted %v", err, d.deleted)
	}

	if err := files.DeleteDropped(ctx, nil, a, []uuid.UUID{x}, []uuid.UUID{x}); err != nil {
		t.Fatalf("nothing dropped without Files = %v", err)
	}
	if err := files.DeleteReplaced(ctx, nil, a, &x, nil); !errors.Is(err, problem.ErrUnavailable) {
		t.Fatalf("dropped without Files = %v, want 503", err)
	}
	d = &deleter{fail: y}
	if err := files.DeleteDropped(ctx, d, a, []uuid.UUID{x, y, z}, nil); !errors.Is(err, errDelete) ||
		!slices.Equal(d.deleted, []uuid.UUID{x}) {
		t.Fatalf("failing Delete = %v, deleted %v", err, d.deleted)
	}
}

// TestDropped (BE-158): the files of before that after no longer lists, in
// the order of before; nothing when every file stays.
func TestDropped(t *testing.T) {
	t.Parallel()
	x, y, z := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	if got := files.Dropped([]uuid.UUID{x, y, z}, []uuid.UUID{z, uuid.NewV7()}); !slices.Equal(got, []uuid.UUID{x, y}) {
		t.Fatalf("Dropped = %v, want %v", got, []uuid.UUID{x, y})
	}
	if got := files.Dropped([]uuid.UUID{x}, []uuid.UUID{x}); len(got) != 0 {
		t.Fatalf("Dropped of a kept file = %v", got)
	}
}
