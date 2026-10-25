package identity

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
)

// Student verification (01 §4.6, 02 §5.4, 04 §2.1; BE-018).
//
//   - .edu.tr path: startEduVerification resolves the address's domain to a
//     university (hooks.domainResolver; directory) and mails a derived,
//     single-use token (24 h) to the address; confirmEduVerification turns
//     it into a verified row valid for one year. No row exists before the
//     confirmation, so nobody can reserve someone else's address by
//     starting a verification with it.
//   - document path (açıköğretim, schools without .edu.tr): an edu_document
//     upload (files: ClamAV + metadata removal) goes to the platform staff
//     queue as a pending row; support or platform_admin approve or reject it,
//     never their own (03 Y6, edu_verifications_reviewer_chk). Every
//     download of a document is audited and its URL lives ≤ 5 minutes.
//   - A renewal (either path) supersedes the previous verification in the
//     same transaction; a pending document is superseded by any new
//     verification. identity.edu_expire lapses verifications whose year
//     ran out (user.edu_verification_lapsed: out of company discovery,
//     audiences and club_admin, 01 §6.2); identity.edu_renewal_reminders
//     warns 30, 7 and 1 days before.
//
// The .edu.tr address is personal: answers show it masked, logs never.
const (
	resourceEdu = "edu_verification"
	eduOwner    = "identity" // files.Owner.Module of student documents
	eduAggType  = "edu_verification"
	// eduDocumentTTL is the lifetime of a document download URL (04 §2.1:
	// presigned ≤ 5 minutes).
	eduDocumentTTL  = 5 * time.Minute
	eduDomainSuffix = ".edu.tr"
	maxReviewNote   = 500
)

// EduDocumentSubmission is the body of submitEduDocument.
type EduDocumentSubmission struct {
	UniversityID uuid.UUID // the university the student claims
	FileID       uuid.UUID // an edu_document upload of the caller
}

// EduDecision is the body of adminDecideEduVerification.
type EduDecision struct {
	Decision     EduStatus  // EduVerified or EduRejected
	UniversityID *uuid.UUID // required to approve: the university the reviewer confirms
	ReviewNote   string     // optional, ≤ 500 characters; shown to the student on a rejection
}

// EduView is a verification with what the answers show next to it.
type EduView struct {
	Record         EduRecord
	UniversityName string       // "" when unknown (Deps.UniversityNames)
	User           *UserSummary // admin answers only
}

// MyEdu is getMyEduVerification. Status: none, pending (a document in
// review), verified, lapsed (the last verification ran out) or rejected
// (the last document was refused). Current is the verification the status
// describes; PendingDocument a document in review next to a verified one
// (renewal).
type MyEdu struct {
	Status          string
	Current         *EduView
	PendingDocument *EduView
}

// EduPage is one page of the admin queue.
type EduPage struct {
	Items []EduView
	More  bool
}

// EduDocumentLink is the answer of adminGetEduDocument.
type EduDocumentLink struct {
	URL       string
	ExpiresAt time.Time
}

// eduResource is the authz view of a verification (03 §3.5: owner = the
// applicant, Attrs status).
func eduResource(v EduRecord) authz.Resource {
	owner := v.UserID
	return authz.Resource{Type: authz.TypeEduVerification, ID: v.ID, OwnerUserID: &owner,
		Attrs: map[string]string{authz.AttrStatus: string(v.Status)}}
}

// eduGate is the decision taken before a verification is read (03 §3.2: a
// caller without the action learns nothing about the ID). The applicant is
// not known yet (nobody); the real resource is decided again once read.
func eduGate(id uuid.UUID) authz.Resource {
	return eduResource(EduRecord{ID: id, UserID: uuid.Nil()})
}

// parseEduEmail validates a .edu.tr address and returns it with its
// normalized form and its (ASCII lower-case) domain.
func parseEduEmail(raw, location string) (email, normalized, domain string, err error) {
	email, normalized, err = parseEmail(raw, location)
	if err != nil {
		return "", "", "", err
	}
	_, d, _ := strings.Cut(email, "@")
	domain = strings.ToLower(d)
	if !strings.HasSuffix(domain, eduDomainSuffix) || len(domain) == len(eduDomainSuffix) {
		return "", "", "", invalid(ErrEduDomainUnknown, location, "must be a .edu.tr address; otherwise submit a student document")
	}
	for _, r := range domain {
		if r > 0x7e {
			return "", "", "", invalid(ErrEduDomainUnknown, location, "must be a .edu.tr address; otherwise submit a student document")
		}
	}
	return email, normalized, domain, nil
}

// resolveDomain maps a .edu.tr domain to its university. Without a
// resolver (directory not wired) every domain is unknown: the safe default
// keeps the student role closed and points at the document path.
func (s *service) resolveDomain(ctx context.Context, domain, location string) (uuid.UUID, error) {
	unknown := invalid(ErrEduDomainUnknown, location, "is not an address of a known university; submit a student document instead")
	if s.d.Domains == nil {
		return uuid.Nil(), unknown
	}
	id, found, err := s.d.Domains(ctx, domain)
	switch {
	case err != nil:
		return uuid.Nil(), err
	case !found || id == uuid.Nil():
		return uuid.Nil(), unknown
	}
	return id, nil
}

// StartEduVerification implements Service (startEduVerification, 202).
// auth_email applies per address, per (client IP, address) and per user
// (a burst against one inbox, from one IP or from one account all stop).
// An address that already verifies another account gets nothing and the
// answer is the same: only the address's holder can find out, by
// confirming (no enumeration of verified addresses).
func (s *service) StartEduVerification(ctx context.Context, a authz.Actor, raw string) error {
	if err := authz.Can(ctx, a, authz.IdentityEduVerificationStart, ownUser(a)); err != nil {
		return err
	}
	email, normalized, domain, err := parseEduEmail(raw, "body.edu_email")
	if err != nil {
		return err
	}
	if err := s.limitEmail(ctx, ruleAuthEmail, normalized); err != nil {
		return err
	}
	if err := s.allow(ctx, ruleAuthEmail, "user:"+a.UserID.String()+":edu"); err != nil {
		return err
	}
	uni, err := s.resolveDomain(ctx, domain, "body.edu_email")
	if err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.d.Store.User(ctx, a.UserID)
		if err != nil {
			return err
		}
		meta := map[string]any{"university_id": uni.String()}
		holder, taken, err := s.d.Store.OpenEduVerificationOfEmail(ctx, normalized)
		if err != nil {
			return err
		}
		if taken && holder.UserID != u.ID {
			meta["result"] = "address_taken"
		} else {
			t, err := s.issueToken(ctx, u, PurposeEduEmail, email)
			if err != nil {
				return err
			}
			meta["token_id"] = t.ID.String()
		}
		return s.d.Audit.Log(ctx, event(authz.IdentityEduVerificationStart, audit.Success, resourceUser, u.ID, meta).By(a))
	})
}

// ConfirmEduVerification implements Service (confirmEduVerification, 204).
// The token is single use and expires after 24 hours; the domain is
// resolved again (a university removed meanwhile is 422).
func (s *service) ConfirmEduVerification(ctx context.Context, a authz.Actor, raw string) error {
	if err := authz.Can(ctx, a, authz.IdentityEduVerificationConfirm, publicUser); err != nil {
		return err
	}
	t, u, err := s.findToken(ctx, raw, PurposeEduEmail)
	if err != nil {
		return err
	}
	if u.Status != StatusActive {
		return ErrInvalidToken
	}
	email, normalized, domain, err := parseEduEmail(t.TargetEmail, "body.token")
	if err != nil {
		return ErrInvalidToken
	}
	uni, err := s.resolveDomain(ctx, domain, "body.token")
	if err != nil {
		return err
	}
	var v EduRecord
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.consumeToken(ctx, t.ID, PurposeEduEmail)
		if err != nil {
			return err
		}
		if cur.Status != StatusActive {
			return ErrInvalidToken
		}
		v, err = s.completeEduEmail(ctx, cur, t.ID, eduAddress{email: email, normalized: normalized, domain: domain, uni: uni}, nil)
		return err
	})
	if err != nil {
		return err
	}
	s.logVerified(ctx, v)
	return nil
}

// eduAddress is a proven .edu.tr address with its university.
type eduAddress struct {
	email, normalized, domain string
	uni                       uuid.UUID
}

// completeEduEmail writes the verified .edu.tr row of cur (locked in the
// caller's transaction, whose address addr a token just proved): the open
// verifications are superseded, the new one is valid for one year and
// user.edu_verified (O) is written with it. confirmEduVerification and
// the registration path of confirmEmail (BE-097) share it, so both follow
// the same rules. extra adds audit metadata.
func (s *service) completeEduEmail(ctx context.Context, cur User, tokenID uuid.UUID, addr eduAddress, extra map[string]any) (EduRecord, error) {
	now := s.now()
	renewal, superseded, err := s.supersedeOpen(ctx, cur.ID, uuid.Nil(), now)
	if err != nil {
		return EduRecord{}, err
	}
	exp, uni := now.AddDate(eduValidityYears, 0, 0), addr.uni
	v := EduRecord{
		ID: uuid.NewV7(), UserID: cur.ID, Method: EduMethodEmail, EduEmail: addr.email, EduEmailNormalized: addr.normalized,
		EmailDomain: addr.domain, UniversityID: &uni, Status: EduVerified, VerifiedAt: &now, ExpiresAt: &exp,
	}
	if err := s.d.Store.InsertEduVerification(ctx, v); err != nil {
		return EduRecord{}, err
	}
	if err := s.emitVerified(ctx, v, renewal); err != nil {
		return EduRecord{}, err
	}
	meta := map[string]any{"token_id": tokenID.String(), "university_id": uni.String(), "renewal": renewal, "superseded": superseded}
	maps.Copy(meta, extra)
	if err := s.d.Audit.Log(ctx, byUser(event(authz.IdentityEduVerificationConfirm, audit.Success, resourceEdu, v.ID, meta), cur.ID)); err != nil {
		return EduRecord{}, err
	}
	return v, nil
}

// logVerified logs a committed student verification (IDs only).
func (s *service) logVerified(ctx context.Context, v EduRecord) {
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "student verified", slog.String("user_id", v.UserID.String()),
		slog.String("verification_id", v.ID.String()), slog.String("method", string(v.Method)))
}

// supersedeOpen ends the open verifications of userID except keep (in the
// caller's transaction, before a new verified row is written): a verified
// row and a pending document become superseded. renewal reports whether
// a verification still in effect was replaced.
func (s *service) supersedeOpen(ctx context.Context, userID, keep uuid.UUID, now time.Time) (renewal bool, ids []string, err error) {
	open, err := s.d.Store.LockOpenEduVerifications(ctx, userID)
	if err != nil {
		return false, nil, err
	}
	ids = []string{}
	for _, o := range open {
		if o.ID == keep {
			continue
		}
		renewal = renewal || o.activeAt(now)
		o.Status = EduSuperseded
		if err := s.d.Store.UpdateEduVerification(ctx, o); err != nil {
			return false, nil, err
		}
		ids = append(ids, o.ID.String())
	}
	return renewal, ids, nil
}

// emitVerified writes user.edu_verified (O) in the transaction of the
// verification (students: audiences; talent: profile refresh on a renewal).
func (s *service) emitVerified(ctx context.Context, v EduRecord, renewal bool) error {
	return outbox.Emit(ctx, s.d.Outbox, domainevent.UserEduVerified{UserID: v.UserID, UniversityID: *v.UniversityID,
		ExpiresAt: *v.ExpiresAt, Renewal: renewal}, eduAggType, v.ID)
}

// usableDocument reports whether f can become a student document of
// userID: an edu_document the user uploaded, verified (active), not found
// infected and not attached elsewhere.
func usableDocument(f files.FileMeta, userID uuid.UUID) bool {
	return f.Purpose == files.PurposeEduDocument && f.UploadedBy != nil && *f.UploadedBy == userID &&
		f.Status == files.StatusActive && f.ScanStatus != files.ScanInfected && f.AttachedModule == ""
}

// SubmitEduDocument implements Service (submitEduDocument, 202): a pending
// row with the document attached, in one transaction. One document per
// user is in review at a time (409 identity.edu_verification_pending).
func (s *service) SubmitEduDocument(ctx context.Context, a authz.Actor, in EduDocumentSubmission) (EduView, error) {
	if err := authz.Can(ctx, a, authz.IdentityEduVerificationSubmitDocument, ownUser(a)); err != nil {
		return EduView{}, err
	}
	if s.d.Files == nil {
		return EduView{}, ErrUnavailable
	}
	if in.UniversityID == uuid.Nil() {
		return EduView{}, invalid(ErrUniversityUnknown, "body.university_id", "must be a university")
	}
	bad := invalid(ErrInvalidEduDocument, "body.document_file_id", "must be an edu_document upload of yours that is complete and unused")
	f, err := s.d.Files.Get(ctx, in.FileID)
	switch {
	case errors.Is(err, problem.ErrNotFound):
		return EduView{}, bad
	case err != nil:
		return EduView{}, err
	case !usableDocument(f, a.UserID):
		return EduView{}, bad
	}
	var v EduRecord
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.d.Store.LockUser(ctx, a.UserID); err != nil {
			return err
		}
		open, err := s.d.Store.LockOpenEduVerifications(ctx, a.UserID)
		if err != nil {
			return err
		}
		for _, o := range open {
			if o.Status == EduPending {
				return ErrEduPending
			}
		}
		uni, file := in.UniversityID, in.FileID
		v = EduRecord{ID: uuid.NewV7(), UserID: a.UserID, Method: EduMethodDocument, UniversityID: &uni,
			DocumentFileID: &file, Status: EduPending}
		if err := s.d.Store.InsertEduVerification(ctx, v); err != nil {
			return err
		}
		err = s.d.Files.Attach(ctx, a, file, files.Owner{Module: eduOwner, EntityID: v.ID, Purpose: files.PurposeEduDocument})
		switch {
		case errors.Is(err, files.ErrAlreadyAttached), errors.Is(err, files.ErrRejected), errors.Is(err, files.ErrNotActive),
			errors.Is(err, files.ErrPurposeMismatch), errors.Is(err, problem.ErrNotFound):
			return bad
		case err != nil:
			return err
		}
		return s.d.Audit.Log(ctx, event(authz.IdentityEduVerificationSubmitDocument, audit.Success, resourceEdu, v.ID,
			map[string]any{"file_id": file.String(), "university_id": uni.String()}).By(a))
	})
	if err != nil {
		return EduView{}, err
	}
	if v, err = s.d.Store.EduVerification(ctx, v.ID); err != nil { // created_at as stored (BE-069)
		return EduView{}, err
	}
	return s.eduView(ctx, v, false)
}

// MyEduVerification implements Service (getMyEduVerification).
func (s *service) MyEduVerification(ctx context.Context, a authz.Actor) (MyEdu, error) {
	if err := authz.Can(ctx, a, authz.IdentityEduVerificationReadOwn, ownUser(a)); err != nil {
		return MyEdu{}, err
	}
	rows, err := s.d.Store.EduVerificationsOf(ctx, a.UserID)
	if err != nil {
		return MyEdu{}, err
	}
	now := s.now()
	var active, pending, last *EduRecord
	for i := range rows { // newest first
		r := &rows[i]
		switch {
		case r.activeAt(now):
			active = cmpOr(active, r)
		case r.Status == EduPending:
			pending = cmpOr(pending, r)
		case r.Status != EduSuperseded:
			last = cmpOr(last, r) // rejected, expired, revoked, or verified past its expiry
		}
	}
	out := MyEdu{Status: "none"}
	var cur *EduRecord
	switch {
	case active != nil:
		out.Status, cur = string(EduVerified), active
	case pending != nil:
		out.Status, cur = string(EduPending), pending
	case last != nil && last.Status == EduRejected:
		out.Status, cur = string(EduRejected), last
	case last != nil:
		out.Status, cur = string(authz.StudentLapsed), last
	}
	if cur != nil {
		v, err := s.eduView(ctx, *cur, false)
		if err != nil {
			return MyEdu{}, err
		}
		out.Current = &v
	}
	if active != nil && pending != nil {
		v, err := s.eduView(ctx, *pending, false)
		if err != nil {
			return MyEdu{}, err
		}
		out.PendingDocument = &v
	}
	return out, nil
}

// cmpOr keeps the first non-nil value (rows are newest first).
func cmpOr(cur, next *EduRecord) *EduRecord {
	if cur != nil {
		return cur
	}
	return next
}

// eduView completes one verification (university name; the applicant on
// admin answers).
func (s *service) eduView(ctx context.Context, v EduRecord, withUser bool) (EduView, error) {
	views, err := s.eduViews(ctx, []EduRecord{v}, withUser)
	if err != nil {
		return EduView{}, err
	}
	return views[0], nil
}

func (s *service) eduViews(ctx context.Context, rows []EduRecord, withUsers bool) ([]EduView, error) {
	out := make([]EduView, len(rows))
	var unis, users []uuid.UUID
	for i, r := range rows {
		out[i].Record = r
		if r.UniversityID != nil {
			unis = append(unis, *r.UniversityID)
		}
		users = append(users, r.UserID)
	}
	if s.d.UniversityNames != nil && len(unis) > 0 {
		names, err := s.d.UniversityNames(ctx, unis)
		if err != nil {
			return nil, err
		}
		for i, r := range rows {
			if r.UniversityID != nil {
				out[i].UniversityName = names[*r.UniversityID]
			}
		}
	}
	if withUsers {
		got, err := s.GetMany(ctx, users)
		if err != nil {
			return nil, err
		}
		for i, r := range rows {
			if u, ok := got[r.UserID]; ok {
				out[i].User = &u
			}
		}
	}
	return out, nil
}

// eduStudents is the StudentVerification of identity (Students).
type eduStudents struct{ s *service }

var _ Students = eduStudents{}

func toEduVerification(v EduRecord) EduVerification {
	return EduVerification{UniversityID: *v.UniversityID, VerifiedAt: *v.VerifiedAt, ExpiresAt: *v.ExpiresAt, Method: string(v.Method)}
}

// Active implements StudentVerification.
func (e eduStudents) Active(ctx context.Context, userID uuid.UUID) (EduVerification, bool, error) {
	rows, err := e.s.d.Store.ActiveEduVerifications(ctx, []uuid.UUID{userID}, e.s.now())
	if err != nil || len(rows) == 0 {
		return EduVerification{}, false, err
	}
	return toEduVerification(rows[0]), true, nil
}

// ActiveMany implements StudentVerification.
func (e eduStudents) ActiveMany(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]EduVerification, error) {
	rows, err := e.s.d.Store.ActiveEduVerifications(ctx, userIDs, e.s.now())
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]EduVerification, len(rows))
	for _, r := range rows {
		out[r.UserID] = toEduVerification(r)
	}
	return out, nil
}

// StudentStatus implements Students (03 §2.1): verified while the newest
// verified row is in its year, lapsed once it ran out (before or after
// identity.edu_expire marked it) or was revoked, none otherwise.
func (e eduStudents) StudentStatus(ctx context.Context, userID uuid.UUID) (StudentState, error) {
	v, found, err := e.s.d.Store.LatestEduOutcome(ctx, userID)
	switch {
	case err != nil:
		return StudentState{}, err
	case !found:
		return StudentState{Status: authz.StudentNone}, nil
	case v.activeAt(e.s.now()):
		ev := toEduVerification(v)
		return StudentState{Status: authz.StudentVerified, Active: &ev}, nil
	}
	return StudentState{Status: authz.StudentLapsed}, nil
}
