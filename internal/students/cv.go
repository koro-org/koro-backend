package students

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// CV rules (02 §3.1, §9.6; 04 §1.14).
const (
	// MaxCVs is the number of CVs a student may keep.
	MaxCVs = 3
	// cvTTL is the lifetime of a CV download URL (04 §1.14: ≤ 5 minutes).
	cvTTL = 5 * time.Minute

	// DropRejectedFileTaskName consumes file.rejected (T-176): a CV or an
	// avatar the scan rejected loses its reference and its file.
	DropRejectedFileTaskName = "students.drop_rejected_file"

	// Audit actions of the system paths (03 §4.1 format; not HTTP actions).
	auditCVRejected     = "students.cv.drop_rejected"
	auditAvatarRejected = "students.profile.drop_rejected_avatar"
)

// CVView is a CV with the display name of its file (listMyCvs).
type CVView struct {
	CV       CV
	FileName string
}

// cvResource is the authz view of the caller's own CVs (03 §3.5
// student_cv: access_grant; 06 H-27: OwnerUserID always set). id is the
// CV, or uuid.Nil() for the collection.
func cvResource(a authz.Actor, id uuid.UUID) authz.Resource {
	me := a.UserID
	return authz.Resource{Type: authz.TypeStudentCV, ID: id, OwnerUserID: &me,
		Attrs: map[string]string{authz.AttrAccessGrant: "none"}}
}

// canCV checks an own-CV action (k-uv: 403 students.verification_required).
func (s *service) canCV(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID) error {
	err := authz.Can(ctx, a, act, cvResource(a, id))
	if err != nil && errors.Is(err, authz.ErrForbidden) && a.IsUser() && !a.IsVerifiedStudent() {
		return ErrVerificationRequired
	}
	return err
}

// cvEvent is the audit record of an own-CV action.
func cvEvent(a authz.Actor, act authz.Action, cv CV, meta map[string]any) audit.Event {
	id := cv.ID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["file_id"] = cv.FileID.String()
	return audit.Event{Action: string(act), Outcome: audit.Success, ResourceType: authz.TypeStudentCV, ResourceID: &id,
		Metadata: meta}.By(a)
}

// cvViews adds the file names of cvs (a file that is gone has none).
func (s *service) cvViews(ctx context.Context, cvs []CV) ([]CVView, error) {
	out := make([]CVView, len(cvs))
	for i, c := range cvs {
		out[i] = CVView{CV: c}
		if s.d.Files == nil {
			continue
		}
		switch f, err := s.d.Files.Get(ctx, c.FileID); {
		case err == nil:
			out[i].FileName = f.OriginalName
		case !errors.Is(err, files.ErrNotFound):
			return nil, err
		}
	}
	return out, nil
}

// ListMyCVs is listMyCvs (students.cv.read_own): oldest first, ≤ 3.
func (s *service) ListMyCVs(ctx context.Context, a authz.Actor) ([]CVView, error) {
	if err := s.canCV(ctx, a, authz.StudentsCvReadOwn, uuid.Nil()); err != nil {
		return nil, err
	}
	cvs, err := s.d.Store.CVs(ctx, a.UserID)
	if err != nil {
		return nil, err
	}
	return s.cvViews(ctx, cvs)
}

// CreateMyCV is createMyCv (students.cv.manage; optional Idempotency-Key).
// Order: authz (k-uv 403) → the file is a cv upload of the caller (422) →
// in one transaction: the profile (created on the first write, it is
// locked: concurrent uploads count one after the other), at most three
// CVs (409 students.cv_limit), the row, files.Attach (the file is bound
// to the CV; a scan verdict pending is fine, files serves it only once
// clean), the current mark, the cv section and the audit record. The
// first CV is current whatever makeCurrent says.
func (s *service) CreateMyCV(ctx context.Context, a authz.Actor, fileID uuid.UUID, makeCurrent bool) (CVView, error) {
	if err := s.canCV(ctx, a, authz.StudentsCvManage, uuid.Nil()); err != nil {
		return CVView{}, err
	}
	if s.d.Files == nil {
		return CVView{}, errUnavailable
	}
	bad := problem.Field("body.file_id", "must be a cv upload of yours (PDF)")
	f, err := s.d.Files.Get(ctx, fileID)
	switch {
	case errors.Is(err, files.ErrNotFound):
		return CVView{}, bad
	case err != nil:
		return CVView{}, err
	}
	if f.Purpose != files.PurposeCV || f.UploadedBy == nil || *f.UploadedBy != a.UserID {
		return CVView{}, bad
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		User    uuid.UUID
		File    uuid.UUID
		Current bool
	}{"create_cv", a.UserID, fileID, makeCurrent})
	if err != nil {
		return CVView{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (CV, error) {
		var out CV
		err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			p, err := s.ensureProfile(ctx, a)
			if err != nil {
				return err
			}
			cur, err := s.d.Store.CVs(ctx, a.UserID)
			if err != nil {
				return err
			}
			if len(cur) >= MaxCVs {
				return ErrCVLimit
			}
			if out, err = s.d.Store.InsertCV(ctx, CV{ID: uuid.NewV7(), UserID: a.UserID, FileID: fileID}); err != nil {
				return err
			}
			if err := s.d.Files.Attach(ctx, a, fileID, files.Owner{Module: studentsModule, EntityID: out.ID,
				Purpose: files.PurposeCV}); err != nil {
				return err
			}
			hasCurrent := false
			for _, c := range cur {
				hasCurrent = hasCurrent || c.IsCurrent
			}
			if makeCurrent || !hasCurrent {
				if out, err = s.d.Store.SetCurrentCV(ctx, a.UserID, out.ID); err != nil {
					return err
				}
			}
			if err := s.markSection(ctx, p, SectionCV, true); err != nil {
				return err
			}
			return s.d.Audit.Log(ctx, cvEvent(a, authz.StudentsCvManage, out,
				map[string]any{"op": "create", "is_current": out.IsCurrent}))
		})
		return out, err
	})
	if err != nil {
		return CVView{}, err
	}
	return CVView{CV: out, FileName: f.OriginalName}, nil
}

// SetMyCurrentCV is setMyCurrentCv (students.cv.manage): the CV becomes
// the only current one (the one a company with level 3 access downloads);
// another user's CV is 404. Setting the current CV again changes nothing.
func (s *service) SetMyCurrentCV(ctx context.Context, a authz.Actor, id uuid.UUID) (CVView, error) {
	if err := s.canCV(ctx, a, authz.StudentsCvManage, id); err != nil {
		return CVView{}, err
	}
	var out CV
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		c, err := s.d.Store.GetCV(ctx, a.UserID, id)
		if err != nil {
			return err
		}
		if c.IsCurrent {
			out = c
			return nil
		}
		if out, err = s.d.Store.SetCurrentCV(ctx, a.UserID, id); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, cvEvent(a, authz.StudentsCvManage, out, map[string]any{"op": "set_current"}))
	})
	if err != nil {
		return CVView{}, err
	}
	views, err := s.cvViews(ctx, []CV{out})
	if err != nil {
		return CVView{}, err
	}
	return views[0], nil
}

// DeleteMyCV is deleteMyCv (students.cv.manage): the row and its file are
// hard deleted in one transaction (02 §9.6; the stored objects go after
// the commit). Another user's CV is 404. A deleted current CV leaves no
// current one: no other CV is shared with companies until the student
// picks it.
func (s *service) DeleteMyCV(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	if err := s.canCV(ctx, a, authz.StudentsCvManage, id); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.d.Store.LockProfile(ctx, a.UserID) // no profile: no CV either (404)
		if err != nil {
			return err
		}
		c, err := s.d.Store.GetCV(ctx, a.UserID, id)
		if err != nil {
			return err
		}
		return s.removeCV(ctx, a, p, c, cvEvent(a, authz.StudentsCvManage, c,
			map[string]any{"op": "delete", "was_current": c.IsCurrent}))
	})
}

// removeCV deletes c and its file in the transaction carried by ctx (p is
// the owner's locked profile, or the zero profile without one), updates
// the cv section and writes ev.
func (s *service) removeCV(ctx context.Context, a authz.Actor, p Profile, c CV, ev audit.Event) error {
	ok, err := s.d.Store.DeleteCV(ctx, c.UserID, c.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if err := s.removeFile(ctx, a, c.FileID); err != nil {
		return err
	}
	if p.UserID != uuid.Nil() {
		rest, err := s.d.Store.CVs(ctx, c.UserID)
		if err != nil {
			return err
		}
		if err := s.markSection(ctx, p, SectionCV, len(rest) > 0); err != nil {
			return err
		}
	}
	return s.d.Audit.Log(ctx, ev)
}

// DownloadMyCV is downloadMyCv (students.cv.read_own): a ≤ 5-minute
// signed URL (attachment) of a clean file (409 files.not_clean while the
// scan is pending), and an access audit record for every download.
// Another user's CV is 404.
func (s *service) DownloadMyCV(ctx context.Context, a authz.Actor, id uuid.UUID) (files.Download, error) {
	if err := s.canCV(ctx, a, authz.StudentsCvReadOwn, id); err != nil {
		return files.Download{}, err
	}
	if s.d.Files == nil {
		return files.Download{}, errUnavailable
	}
	c, err := s.d.Store.GetCV(ctx, a.UserID, id)
	if err != nil {
		return files.Download{}, err
	}
	dl, err := s.d.Files.PresignGet(ctx, c.FileID, cvTTL)
	if err != nil {
		return files.Download{}, err
	}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return s.d.Audit.Log(ctx, cvEvent(a, authz.StudentsCvReadOwn, c, map[string]any{"op": "download"}))
	})
	if err != nil {
		return files.Download{}, err
	}
	return dl, nil
}

// handleFileRejected is students.drop_rejected_file (file.rejected, T-176):
// a file attached to students that the scan rejected (malware, or content
// that could not be cleaned) loses its reference and is deleted through
// files. A CV row goes with its file (the cv section follows); an avatar
// is cleared from the profile. Idempotent: a second delivery finds nothing
// and files.Delete of a missing file is no error.
func (s *service) handleFileRejected(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.FileRejected](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.AttachedModule != studentsModule {
		return nil
	}
	sys := authz.SystemActor(DropRejectedFileTaskName)
	switch files.Purpose(p.Purpose) {
	case files.PurposeCV:
		return s.dropRejectedCV(ctx, sys, p)
	case files.PurposeAvatar:
		return s.dropRejectedAvatar(ctx, sys, p)
	}
	s.d.Log.LogAttrs(ctx, slog.LevelWarn, "rejected students file of an unknown purpose kept",
		slog.String("file_id", p.FileID.String()), slog.String("purpose", p.Purpose))
	return nil
}

func (s *service) dropRejectedCV(ctx context.Context, sys authz.Actor, p domainevent.FileRejected) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		c, err := s.d.Store.CVByFile(ctx, p.FileID)
		switch {
		case errors.Is(err, ErrNotFound): // the student deleted it meanwhile: only the file may remain
			return s.removeFile(ctx, sys, p.FileID)
		case err != nil:
			return err
		}
		prof, err := s.d.Store.LockProfile(ctx, c.UserID)
		switch {
		case errors.Is(err, ErrNotFound):
			prof = Profile{}
		case err != nil:
			return err
		}
		return s.removeCV(ctx, sys, prof, c, systemEvent(DropRejectedFileTaskName, auditCVRejected, c.UserID, map[string]any{
			"cv_id": c.ID.String(), "file_id": p.FileID.String(), "reason": p.Reason, "was_current": c.IsCurrent,
		}))
	})
}

func (s *service) dropRejectedAvatar(ctx context.Context, sys authz.Actor, p domainevent.FileRejected) error {
	if p.AttachedEntityID == nil {
		return s.removeFile(ctx, sys, p.FileID)
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		prof, err := s.d.Store.LockProfile(ctx, *p.AttachedEntityID)
		switch {
		case errors.Is(err, ErrNotFound):
			return s.removeFile(ctx, sys, p.FileID)
		case err != nil:
			return err
		}
		if prof.AvatarFileID == nil || *prof.AvatarFileID != p.FileID {
			return s.removeFile(ctx, sys, p.FileID) // replaced meanwhile
		}
		prof.AvatarFileID = nil
		if _, err := s.d.Store.UpdateProfile(ctx, prof); err != nil {
			return err
		}
		if err := s.removeFile(ctx, sys, p.FileID); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, systemEvent(DropRejectedFileTaskName, auditAvatarRejected, prof.UserID,
			map[string]any{"file_id": p.FileID.String(), "reason": p.Reason}))
	})
}
