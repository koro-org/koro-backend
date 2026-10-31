package files

import (
	"context"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/problem"
)

// Deleter is the part of Files an owning module calls to delete the files
// it no longer references (Files.Delete; sync-write files.Files.Delete).
type Deleter interface {
	Delete(ctx context.Context, a authz.Actor, fileID uuid.UUID) error
}

// DeleteDropped deletes, as a in the transaction carried by ctx, every file
// of before that after no longer lists: the images of a post the author
// replaced or removed, or of a deleted draft (BE-155, T-247, T-376). The
// caller has already written its rows without them (the file FKs are not
// deferred), so the file never outlives its reference nor the reference its
// file: an attached file is no orphan, files.orphan_cleanup never takes it.
// Files.Delete removes the objects and the public media copy after the
// commit; a file already gone (a rejected one, a second request) is no
// error. A nil del with something to delete is ErrUnavailable (503): the
// write is refused rather than leaving the file behind.
func DeleteDropped(ctx context.Context, del Deleter, a authz.Actor, before, after []uuid.UUID) error {
	for _, id := range Dropped(before, after) {
		if del == nil {
			return problem.ErrUnavailable
		}
		if err := del.Delete(ctx, a, id); err != nil {
			return err
		}
	}
	return nil
}

// Dropped returns the files of before that after no longer lists, in the
// order of before: what DeleteDropped deletes, or what an owner keeps
// elsewhere instead (feed keeps the images dropped off a published post for
// the moderators, BE-158).
func Dropped(before, after []uuid.UUID) []uuid.UUID {
	var out []uuid.UUID
	for _, id := range before {
		if !slices.Contains(after, id) {
			out = append(out, id)
		}
	}
	return out
}

// DeleteReplaced is DeleteDropped for a single file reference (an org logo,
// an event cover): the old file goes when the new one differs or is nil.
func DeleteReplaced(ctx context.Context, del Deleter, a authz.Actor, before, after *uuid.UUID) error {
	return DeleteDropped(ctx, del, a, opt(before), opt(after))
}

func opt(id *uuid.UUID) []uuid.UUID {
	if id == nil {
		return nil
	}
	return []uuid.UUID{*id}
}
