package identity

import (
	"context"
	"errors"
	"log/slog"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// EduQueue implements Service (adminListEduVerifications): the student
// documents of one status (default pending, oldest first). Applicants are
// shown by name only; documents are opened one by one (audited).
func (s *service) EduQueue(ctx context.Context, a authz.Actor, q EduQuery) (EduPage, error) {
	if err := authz.Can(ctx, a, authz.IdentityEduVerificationList, adminSurface); err != nil {
		return EduPage{}, err
	}
	switch q.Status {
	case "":
		q.Status = EduPending
	case EduPending, EduVerified, EduRejected, EduExpired, EduRevoked, EduSuperseded:
	default:
		return EduPage{}, invalid(problem.ErrValidation, "query.status", "must be a verification status")
	}
	limit := page.ClampLimit(q.Limit)
	q.Limit = limit + 1
	rows, err := s.d.Store.EduQueue(ctx, q)
	if err != nil {
		return EduPage{}, err
	}
	items, more := page.Cut(rows, limit)
	views, err := s.eduViews(ctx, items, true)
	if err != nil {
		return EduPage{}, err
	}
	return EduPage{Items: views, More: more}, nil
}

// reviewable loads a student document verification for a reviewer: the
// gate first (nothing is read before it), then the row (an e-mail
// verification has no document: 404), then the decision on the real
// applicant (03 Y6: nobody reviews their own document).
func (s *service) reviewable(ctx context.Context, a authz.Actor, id uuid.UUID, lock bool) (EduRecord, error) {
	load := s.d.Store.EduVerification
	if lock {
		load = s.d.Store.LockEduVerification
	}
	v, err := load(ctx, id)
	if err != nil {
		return EduRecord{}, err
	}
	if v.Method != EduMethodDocument {
		return EduRecord{}, ErrNotFound
	}
	if err := authz.Can(ctx, a, authz.IdentityEduVerificationReview, eduResource(v)); err != nil {
		return EduRecord{}, err
	}
	return v, nil
}

// EduDocument implements Service (adminGetEduDocument): a download URL of
// the document, valid five minutes, for a clean file only (files.ErrNotClean
// while the scan is pending or found something). Every call is audited
// (02 §5.4: the document is personal) before the URL is handed out.
func (s *service) EduDocument(ctx context.Context, a authz.Actor, id uuid.UUID) (EduDocumentLink, error) {
	if err := authz.Can(ctx, a, authz.IdentityEduVerificationReview, eduGate(id)); err != nil {
		return EduDocumentLink{}, err
	}
	if s.d.Files == nil {
		return EduDocumentLink{}, ErrUnavailable
	}
	v, err := s.reviewable(ctx, a, id, false)
	if err != nil {
		return EduDocumentLink{}, err
	}
	if v.DocumentFileID == nil { // removed by the edu_document retention
		return EduDocumentLink{}, ErrNotFound
	}
	dl, err := s.d.Files.PresignGet(ctx, *v.DocumentFileID, eduDocumentTTL)
	if err != nil {
		return EduDocumentLink{}, err
	}
	err = s.d.Audit.Log(ctx, event(authz.IdentityEduVerificationReview, audit.Success, resourceEdu, v.ID, map[string]any{
		"operation": "document_download", "file_id": v.DocumentFileID.String(), "subject_user_id": v.UserID.String(),
		"status": string(v.Status),
	}).By(a))
	if err != nil {
		return EduDocumentLink{}, err
	}
	return EduDocumentLink{URL: dl.URL, ExpiresAt: dl.ExpiresAt}, nil
}

// DecideEduVerification implements Service (adminDecideEduVerification).
// Approval needs a clean document and an active account; the applicant's
// previous verification (if any) is superseded, user.edu_verified (O) is
// written and the applicant is told. A rejection keeps the note for the
// applicant. A decided verification is 409 identity.edu_invalid_state.
func (s *service) DecideEduVerification(ctx context.Context, a authz.Actor, id uuid.UUID, in EduDecision) (EduView, error) {
	if err := authz.Can(ctx, a, authz.IdentityEduVerificationReview, eduGate(id)); err != nil {
		return EduView{}, err
	}
	approve := in.Decision == EduVerified
	switch {
	case !approve && in.Decision != EduRejected:
		return EduView{}, invalid(problem.ErrValidation, "body.decision", "must be verified or rejected")
	case approve && (in.UniversityID == nil || *in.UniversityID == uuid.Nil()):
		return EduView{}, invalid(ErrUniversityUnknown, "body.university_id", "is required to approve")
	}
	note, err := cleanReviewNote(in.ReviewNote)
	if err != nil {
		return EduView{}, err
	}
	if s.d.Files == nil {
		return EduView{}, ErrUnavailable
	}
	var v EduRecord
	renewal := false
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		if v, err = s.reviewable(ctx, a, id, true); err != nil {
			return err
		}
		if v.Status != EduPending {
			return ErrEduInvalidState
		}
		now, reviewer := s.now(), a.UserID
		v.ReviewedBy, v.ReviewNote, v.UpdatedAt = &reviewer, note, now
		meta := map[string]any{"decision": string(in.Decision), "subject_user_id": v.UserID.String()}
		notice := eduNoticeRejected
		if approve {
			if err := s.approvable(ctx, v); err != nil {
				return err
			}
			var superseded []string
			if renewal, superseded, err = s.supersedeOpen(ctx, v.UserID, v.ID, now); err != nil {
				return err
			}
			exp, uni := now.AddDate(eduValidityYears, 0, 0), *in.UniversityID
			v.Status, v.UniversityID, v.VerifiedAt, v.ExpiresAt = EduVerified, &uni, &now, &exp
			meta["university_id"], meta["renewal"], meta["superseded"] = uni.String(), renewal, superseded
			notice = eduNoticeApproved
		} else {
			v.Status = EduRejected
		}
		if err := s.d.Store.UpdateEduVerification(ctx, v); err != nil {
			return err
		}
		if approve {
			if err := s.emitVerified(ctx, v, renewal); err != nil {
				return err
			}
		}
		s.notifyEdu(ctx, eduNotice{UserID: v.UserID, VerificationID: v.ID, Kind: notice})
		return s.d.Audit.Log(ctx, event(authz.IdentityEduVerificationReview, audit.Success, resourceEdu, v.ID, meta).By(a))
	})
	if err != nil {
		return EduView{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "student document decided", slog.String("verification_id", v.ID.String()),
		slog.String("decision", string(in.Decision)))
	return s.eduView(ctx, v, true)
}

// approvable checks what an approval needs besides the pending state: an
// active applicant (a suspended or erased account is not verified) and a
// document the reviewer could see (clean, or skipped without a scanner;
// files.ErrNotClean otherwise).
func (s *service) approvable(ctx context.Context, v EduRecord) error {
	u, err := s.d.Store.User(ctx, v.UserID)
	if err != nil {
		return err
	}
	if u.Status != StatusActive {
		return ErrUserInvalidState
	}
	if v.DocumentFileID == nil {
		return files.ErrNotClean
	}
	f, err := s.d.Files.Get(ctx, *v.DocumentFileID)
	switch {
	case errors.Is(err, problem.ErrNotFound):
		return files.ErrNotClean
	case err != nil:
		return err
	case f.Status != files.StatusActive || (f.ScanStatus != files.ScanClean && f.ScanStatus != files.ScanSkipped):
		return files.ErrNotClean
	}
	return nil
}

// cleanReviewNote normalizes an optional review note (≤ 500 characters of
// plain text).
func cleanReviewNote(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	n, err := textx.NormalizeText(raw)
	if err != nil || len([]rune(n)) > maxReviewNote {
		return "", invalid(problem.ErrValidation, "body.review_note", "must be at most 500 characters of plain text")
	}
	return n, nil
}
