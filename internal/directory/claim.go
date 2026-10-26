package directory

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Club claim limits (02 §8.6) and the evidence download (≤ 5 minutes,
// 05 §14.7).
const (
	maxRoleInClub   = 100
	maxEvidenceText = 2000
	maxDecisionNote = 1000
	evidenceTTL     = 5 * time.Minute

	// claimOwner is files.Owner.Module of claim evidence.
	claimOwner = "directory"
)

// Outcomes of club_claim.decided (01 §6.2).
const (
	OutcomeApproved = string(ClaimApproved)
	OutcomeRejected = string(ClaimRejected)
)

// Evidence states of an admin claim view.
const (
	EvidenceNone      = "none"      // no evidence file
	EvidenceAvailable = "available" // the download URL is in the view
	EvidenceNotClean  = "not_clean" // the scan has not cleared the file (yet)
)

// ClaimInput is the body of createClubClaim (04 §2.3): the role the
// claimant holds in the club and the evidence (a text, a
// club_claim_evidence upload, or both; at least one).
type ClaimInput struct {
	RoleInClub     string
	EvidenceText   string
	EvidenceFileID *uuid.UUID
}

// ClaimDecision is the body of adminDecideClubClaim.
type ClaimDecision struct {
	Decision ClaimState // approved or rejected
	Note     string     // optional; shown to the reviewers only (never the claimant)
}

// AdminClaimQuery narrows adminListClubClaims.
type AdminClaimQuery struct {
	Status ClaimState // "" → pending
	// UniversityOrg narrows the queue to the clubs of one university (by
	// its org ID): a university panel's own queue (BE-127). A
	// representative may ask only for a university they represent.
	UniversityOrg *uuid.UUID
	After         *uuid.UUID
	Limit         int
}

// ClaimView is a claim as its reader sees it. The claimant's views carry
// the claim and the club's name; the admin views also the club, the
// claimant's name and (detail only) the evidence download.
type ClaimView struct {
	Claim    ClubClaim
	ClubName string
	Club     Club
	// Claimant is the claimant's account summary (admin views; nil when
	// identity is not wired or the account is gone).
	Claimant *identity.UserSummary
	// Evidence is the short-lived download of the evidence file
	// (adminGetClubClaim, when EvidenceState is available).
	Evidence      *files.Download
	EvidenceState string
	// Approver is who decides the claim (adminGetClubClaim, decision):
	// authz.ClaimApproverRep or authz.ClaimApproverPlatformAdmin; "" in
	// lists.
	Approver string
}

// ClaimPage is a page of claims; Next is the cursor position (the last ID).
type ClaimPage struct {
	Items []ClaimView
	Next  *uuid.UUID
}

// approval is who decides a claim (E-21, orchestrator 2026-10-04): the
// university_rep of the club's university when it has one (other than the
// claimant) and the club is not sensitive, a platform admin otherwise.
type approval struct {
	UniversityOrg uuid.UUID
	Approver      string // authz.ClaimApproverRep | authz.ClaimApproverPlatformAdmin
}

// claimResource is the authz view of a read claim (03 §3.5: OrgID = the
// club's org, OwnerUserID = the claimant, Attrs status and the approval).
func claimResource(c ClubClaim, ap approval) authz.Resource {
	org, owner := c.ClubOrgID, c.ClaimantUserID
	return authz.Resource{Type: authz.TypeClubClaim, ID: c.ID, OrgID: &org, OwnerUserID: &owner,
		Attrs: map[string]string{authz.AttrStatus: string(c.Status), authz.AttrClaimUniversityOrg: ap.UniversityOrg.String(),
			authz.AttrClaimApprover: ap.Approver}}
}

// ownResource is the authz view of a claim for its claimant (O); the
// approval does not matter (and is not looked up).
func ownResource(c ClubClaim) authz.Resource {
	org, owner := c.ClubOrgID, c.ClaimantUserID
	return authz.Resource{Type: authz.TypeClubClaim, ID: c.ID, OrgID: &org, OwnerUserID: &owner,
		Attrs: map[string]string{authz.AttrStatus: string(c.Status), authz.AttrClaimUniversityOrg: "",
			authz.AttrClaimApprover: ""}}
}

// claimGate is the decision taken before a claim is read (03 §3.2: a
// caller without the action learns nothing about the ID). The claimant is
// not known yet (nobody); the real claim is decided again once read (Y6).
func claimGate(id uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypeClubClaim, ID: id, OwnerUserID: new(uuid.Nil()),
		Attrs: map[string]string{authz.AttrStatus: "", authz.AttrClaimUniversityOrg: "",
			authz.AttrClaimApprover: authz.ClaimApproverUnread}}
}

// ownClaims is the authz view of the caller's own claims (the list).
func ownClaims(a authz.Actor) authz.Resource {
	owner := a.UserID
	return authz.Resource{Type: authz.TypeClubClaim, OwnerUserID: &owner, Attrs: map[string]string{authz.AttrStatus: ""}}
}

// approvalOf finds the approver of a claim on club created at createdAt:
// the club's university org, and whether that org has an active
// university_rep other than the claimant (orgs reads; a sensitive club
// always goes to platform staff: its claim is special category data,
// 03 Y2). A claim the rep has not decided within ClaimEscalateAfter goes
// to platform staff (06 H-58).
func (s *service) approvalOf(ctx context.Context, club Club, claimant uuid.UUID, createdAt time.Time) (approval, error) {
	ap, reps, err := s.repApproval(ctx, club, claimant)
	if err != nil {
		return approval{}, err
	}
	if len(reps) > 0 && !escalated(createdAt, s.now()) {
		ap.Approver = authz.ClaimApproverRep
	}
	return ap, nil
}

// repApproval returns the platform-staff approval of a claim on club and
// the active university_reps of the club's university other than the
// claimant who would decide it instead (none for a sensitive club).
func (s *service) repApproval(ctx context.Context, club Club, claimant uuid.UUID) (approval, []uuid.UUID, error) {
	u, err := s.d.Store.GetUniversity(ctx, club.UniversityID)
	if err != nil {
		return approval{}, nil, err
	}
	ap := approval{UniversityOrg: u.OrgID, Approver: authz.ClaimApproverPlatformAdmin}
	if club.IsSensitive {
		return ap, nil, nil
	}
	ids, err := s.d.Orgs.ActiveMemberIDs(ctx, u.OrgID)
	if err != nil {
		return approval{}, nil, err
	}
	var reps []uuid.UUID
	for _, id := range ids {
		if id == claimant {
			continue
		}
		ok, err := s.d.Orgs.HasRole(ctx, u.OrgID, id, authz.MemberUniversityRep)
		if err != nil {
			return approval{}, nil, err
		}
		if ok {
			reps = append(reps, id)
		}
	}
	return ap, reps, nil
}

// repUniversities returns the university orgs where a is a
// university_rep (the scope of a rep's review queue).
func repUniversities(a authz.Actor) []uuid.UUID {
	var out []uuid.UUID
	for _, m := range a.Memberships {
		if m.Role == authz.MemberUniversityRep {
			out = append(out, m.OrgID)
		}
	}
	return out
}

// cleanMultiline normalises a multi-line plain text: every line as
// textx.NormalizeText, at most one empty line in a row, no leading or
// trailing empty lines; "" (or only spaces) is nil.
func cleanMultiline(loc, s string, maxRunes int) (*string, error) {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		n, err := textx.NormalizeText(ln)
		if err != nil {
			return nil, problem.Field(loc, "contains control or bidi characters")
		}
		if n == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, n)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return nil, nil
	}
	v := strings.Join(out, "\n")
	if utf8.RuneCountInString(v) > maxRunes {
		return nil, problem.Field(loc, "must be at most "+strconv.Itoa(maxRunes)+" characters")
	}
	return &v, nil
}

// usableEvidence: the caller's own complete club_claim_evidence upload,
// not infected and not attached elsewhere.
func usableEvidence(f files.FileMeta, userID uuid.UUID) bool {
	return f.Purpose == files.PurposeClubClaimEvidence && f.UploadedBy != nil && *f.UploadedBy == userID &&
		f.Status == files.StatusActive && f.ScanStatus != files.ScanInfected && f.AttachedModule == ""
}

// claimAudit is the audit record of a claim action. Only the approval is
// org-scoped (the club's admins see who became an admin); a claim itself
// is the claimant's personal record, kept out of the club's audit view.
func claimAudit(a authz.Actor, act authz.Action, c ClubClaim, club Club, orgScoped bool, meta map[string]any) audit.Event {
	if meta == nil {
		meta = map[string]any{}
	}
	meta["club_org_id"] = c.ClubOrgID.String()
	meta[audit.MetaSubjectUserID] = c.ClaimantUserID.String()
	if club.IsSensitive {
		meta[audit.MetaSensitive] = true
	}
	var org *uuid.UUID
	if orgScoped {
		org = &c.ClubOrgID
	}
	return auditEvent(a, act, authz.TypeClubClaim, c.ID, org, "", meta)
}

// CreateClubClaim implements Service. Order: the club (404) → authz (403
// students.verification_required without a student verification,
// directory.minor_restricted for a minor; 03 k8) → the club's org (closed
// 409, suspended 403) → the caller already runs the club (409) → the
// university of the caller's verification (422, k8) → the body (422) → the
// evidence file → in one transaction: the claim (409 for a second pending
// claim), the evidence attached, the audit record.
func (s *service) CreateClubClaim(ctx context.Context, a authz.Actor, clubID uuid.UUID, in ClaimInput) (ClaimView, error) {
	c, err := s.GetClub(ctx, clubID)
	if err != nil {
		return ClaimView{}, err
	}
	if err := authz.Can(ctx, a, authz.DirectoryClubClaimCreate, clubResource(c)); err != nil {
		if errors.Is(err, authz.ErrForbidden) && a.IsUser() {
			switch {
			case !a.IsVerifiedStudent():
				return ClaimView{}, orgs.ErrVerificationRequired // 03 §2.3, k-uv (H-52: the orgs value)
			case a.IsMinor:
				return ClaimView{}, ErrMinorRestricted // 03 k8, Y4
			}
		}
		return ClaimView{}, err
	}
	if err := s.writableOrg(ctx, a, c.OrgID); err != nil {
		return ClaimView{}, err
	}
	if a.HasOrgRole(c.OrgID, authz.MemberClubAdmin) {
		return ClaimView{}, ErrInvalidState // already runs the club
	}
	if a.UniversityID == nil || *a.UniversityID != c.UniversityID {
		return ClaimView{}, ErrUniversityMismatch
	}
	role, err := cleanText("body.role_in_club", in.RoleInClub, 2, maxRoleInClub)
	if err != nil {
		return ClaimView{}, err
	}
	text, err := cleanMultiline("body.evidence_text", in.EvidenceText, maxEvidenceText)
	if err != nil {
		return ClaimView{}, err
	}
	if text == nil && in.EvidenceFileID == nil {
		return ClaimView{}, problem.Field("body.evidence_text", "evidence_text or evidence_file_id is required")
	}
	badFile := problem.Field("body.evidence_file_id", "must be a club_claim_evidence upload of yours that is complete and unused")
	if in.EvidenceFileID != nil {
		if s.d.Files == nil {
			return ClaimView{}, errUnavailable
		}
		f, err := s.d.Files.Get(ctx, *in.EvidenceFileID)
		switch {
		case errors.Is(err, problem.ErrNotFound):
			return ClaimView{}, badFile
		case err != nil:
			return ClaimView{}, err
		case !usableEvidence(f, a.UserID):
			return ClaimView{}, badFile
		}
	}
	hash, err := idempotency.Hash(struct {
		Op   string
		Club uuid.UUID
		Role string
		Text string
		File *uuid.UUID
	}{"create_claim", c.OrgID, role, deref(text), in.EvidenceFileID})
	if err != nil {
		return ClaimView{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		claim, err := s.d.Store.InsertClaim(ctx, ClubClaim{
			ID: uuid.NewV7(), ClubOrgID: c.OrgID, ClaimantUserID: a.UserID, Status: ClaimPending, RoleInClub: role,
			EvidenceText: text, EvidenceFileID: in.EvidenceFileID,
		})
		if err != nil {
			return uuid.Nil(), err
		}
		if f := in.EvidenceFileID; f != nil {
			err := s.d.Files.Attach(ctx, a, *f, files.Owner{Module: claimOwner, EntityID: claim.ID, Purpose: files.PurposeClubClaimEvidence})
			switch {
			case errors.Is(err, files.ErrAlreadyAttached), errors.Is(err, files.ErrRejected), errors.Is(err, files.ErrNotActive),
				errors.Is(err, files.ErrPurposeMismatch), errors.Is(err, problem.ErrNotFound):
				return uuid.Nil(), badFile
			case err != nil:
				return uuid.Nil(), err
			}
		}
		// The rep who decides it is told after the commit (06 H-58; the
		// task re-reads the claim, so a withdrawn one notifies nobody).
		s.d.Queue.AfterCommit(ctx, ClaimCreatedTaskName, claimNotice{ClaimID: claim.ID}, ClaimCreatedTaskName+":"+claim.ID.String())
		return claim.ID, s.d.Audit.Log(ctx, claimAudit(a, authz.DirectoryClubClaimCreate, claim, c, false,
			map[string]any{"evidence_file": in.EvidenceFileID != nil, "evidence_text": text != nil}))
	})
	if err != nil {
		return ClaimView{}, err
	}
	claim, err := s.d.Store.GetClaim(ctx, id)
	if err != nil {
		return ClaimView{}, err
	}
	return s.ownView(ctx, claim)
}

// ownView is a claim as its claimant sees it (no decision note).
func (s *service) ownView(ctx context.Context, c ClubClaim) (ClaimView, error) {
	names, err := s.clubNames(ctx, []ClubClaim{c})
	if err != nil {
		return ClaimView{}, err
	}
	return ClaimView{Claim: c, ClubName: names[c.ClubOrgID]}, nil
}

// clubNames returns the org names of the claims' clubs.
func (s *service) clubNames(ctx context.Context, cs []ClubClaim) (map[uuid.UUID]string, error) {
	ids := make([]uuid.UUID, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.ClubOrgID)
	}
	out := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	sums, err := s.d.Orgs.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, o := range sums {
		out[id] = o.Name
	}
	return out, nil
}

// ListMyClubClaims implements Service: the caller's claims, newest first.
func (s *service) ListMyClubClaims(ctx context.Context, a authz.Actor, after *uuid.UUID, limit int) (ClaimPage, error) {
	if err := authz.Can(ctx, a, authz.DirectoryClubClaimReadOwn, ownClaims(a)); err != nil {
		return ClaimPage{}, err
	}
	limit = page.ClampLimit(limit)
	rows, err := s.d.Store.ClaimsOf(ctx, a.UserID, after, limit+1)
	if err != nil {
		return ClaimPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	names, err := s.clubNames(ctx, rows)
	if err != nil {
		return ClaimPage{}, err
	}
	out := ClaimPage{Items: make([]ClaimView, len(rows))}
	for i, c := range rows {
		out.Items[i] = ClaimView{Claim: c, ClubName: names[c.ClubOrgID]}
	}
	if more {
		out.Next = new(rows[len(rows)-1].ID)
	}
	return out, nil
}

// WithdrawMyClubClaim implements Service. Order: the claim (404) → authz
// (another user's claim is 404, O) → pending only (409
// directory.claim_state) → withdrawn + audit in one transaction.
func (s *service) WithdrawMyClubClaim(ctx context.Context, a authz.Actor, id uuid.UUID) (ClaimView, error) {
	c, err := s.d.Store.GetClaim(ctx, id)
	if err != nil {
		return ClaimView{}, err
	}
	if err := authz.Can(ctx, a, authz.DirectoryClubClaimWithdraw, ownResource(c)); err != nil {
		return ClaimView{}, err
	}
	var out ClubClaim
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		c, err := s.d.Store.LockClaim(ctx, id)
		if err != nil {
			return err
		}
		if c.Status != ClaimPending {
			return ErrClaimState
		}
		c.Status = ClaimWithdrawn
		if out, err = s.d.Store.UpdateClaim(ctx, c); err != nil {
			return err
		}
		club, err := s.d.Store.GetClub(ctx, c.ClubOrgID)
		if err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, claimAudit(a, authz.DirectoryClubClaimWithdraw, out, club, false, nil))
	})
	if err != nil {
		return ClaimView{}, err
	}
	return s.ownView(ctx, out)
}

// adminViews builds the admin views of claims: club rows, names and the
// claimants' account summaries.
func (s *service) adminViews(ctx context.Context, cs []ClubClaim) ([]ClaimView, error) {
	names, err := s.clubNames(ctx, cs)
	if err != nil {
		return nil, err
	}
	users := map[uuid.UUID]identity.UserSummary{}
	if s.d.Users != nil && len(cs) > 0 {
		ids := make([]uuid.UUID, 0, len(cs))
		for _, c := range cs {
			ids = append(ids, c.ClaimantUserID)
		}
		if users, err = s.d.Users.GetMany(ctx, ids); err != nil {
			return nil, err
		}
	}
	out := make([]ClaimView, len(cs))
	for i, c := range cs {
		club, err := s.d.Store.GetClub(ctx, c.ClubOrgID)
		if err != nil {
			return nil, err
		}
		v := ClaimView{Claim: c, ClubName: names[c.ClubOrgID], Club: club, EvidenceState: EvidenceNone}
		if u, ok := users[c.ClaimantUserID]; ok {
			v.Claimant = &u
		}
		out[i] = v
	}
	return out, nil
}

// AdminListClubClaims implements Service: authz (403 before reading;
// support and platform_admin see every claim, a university_rep the claims
// they approve: their universities' non-sensitive clubs; university_org_id
// narrows either to one university, and a rep's scope is checked on that
// university only: not theirs → 404 as an invisible claim) → status → the
// page → one audit record of the read (claims are personal data; a
// sensitive club's claims are special category data).
func (s *service) AdminListClubClaims(ctx context.Context, a authz.Actor, q AdminClaimQuery) (ClaimPage, error) {
	f := ClaimFilter{Status: q.Status}
	if err := authz.Can(ctx, a, authz.DirectoryClubClaimList, adminSurface); err != nil {
		reps := repUniversities(a)
		if uni := q.UniversityOrg; uni != nil && len(reps) > 0 {
			reps = []uuid.UUID{*uni} // authz below checks the rep's role on it
		}
		if !errors.Is(err, authz.ErrForbidden) || len(reps) == 0 {
			return ClaimPage{}, err
		}
		for _, uni := range reps {
			scope := authz.Resource{Type: authz.TypeClubClaim, OwnerUserID: new(uuid.Nil()), Attrs: map[string]string{
				authz.AttrStatus: "", authz.AttrClaimUniversityOrg: uni.String(), authz.AttrClaimApprover: authz.ClaimApproverRep,
			}}
			if err := authz.Can(ctx, a, authz.DirectoryClubClaimList, scope); err != nil {
				return ClaimPage{}, err // e.g. 403 authz.mfa_required
			}
		}
		f.RepUniversityOrgs = reps
		// A pending claim older than ClaimEscalateAfter is platform staff's
		// (06 H-58); decided claims stay in the rep's history.
		f.RepPendingSince = new(s.now().Add(-ClaimEscalateAfter))
	} else {
		f.UniversityOrg = q.UniversityOrg // platform staff: every club of it, sensitive ones included
	}
	if f.Status == "" {
		f.Status = ClaimPending
	}
	if !f.Status.Valid() {
		return ClaimPage{}, problem.Field("query.status", "must be pending, approved, rejected or withdrawn")
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListClaims(ctx, f, q.After, limit+1)
	if err != nil {
		return ClaimPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	views, err := s.adminViews(ctx, rows)
	if err != nil {
		return ClaimPage{}, err
	}
	sensitive := false
	ids := make([]string, len(views))
	for i, v := range views {
		ids[i] = v.Claim.ID.String()
		sensitive = sensitive || v.Club.IsSensitive
	}
	meta := map[string]any{"operation": "queue", authz.AttrStatus: string(f.Status), "claim_ids": ids}
	if f.RepUniversityOrgs != nil {
		meta["scope"] = authz.ClaimApproverRep
	}
	if q.UniversityOrg != nil {
		meta[authz.AttrClaimUniversityOrg] = q.UniversityOrg.String()
	}
	if sensitive {
		meta[audit.MetaSensitive] = true
	}
	err = s.d.Audit.Log(ctx, audit.Event{
		Action: string(authz.DirectoryClubClaimList), Outcome: audit.Success, ResourceType: authz.TypeClubClaim, Metadata: meta,
	}.By(a))
	if err != nil {
		return ClaimPage{}, err
	}
	out := ClaimPage{Items: views}
	if more {
		out.Next = new(rows[len(rows)-1].ID)
	}
	return out, nil
}

// AdminGetClubClaim implements Service: authz on the gate (403 before
// reading for everyone but support, platform_admin and university reps) →
// the claim (404) → authz on the claim (a rep reads only the claims they
// approve, others 404) → the evidence download of a clean file (5 minutes)
// → the audit record of the read, before the view is handed out.
func (s *service) AdminGetClubClaim(ctx context.Context, a authz.Actor, id uuid.UUID) (ClaimView, error) {
	if err := authz.Can(ctx, a, authz.DirectoryClubClaimList, claimGate(id)); err != nil {
		return ClaimView{}, err
	}
	c, err := s.d.Store.GetClaim(ctx, id)
	if err != nil {
		return ClaimView{}, err
	}
	club, err := s.d.Store.GetClub(ctx, c.ClubOrgID)
	if err != nil {
		return ClaimView{}, err
	}
	ap, err := s.approvalOf(ctx, club, c.ClaimantUserID, c.CreatedAt)
	if err != nil {
		return ClaimView{}, err
	}
	if err := authz.Can(ctx, a, authz.DirectoryClubClaimList, claimResource(c, ap)); err != nil {
		return ClaimView{}, err
	}
	views, err := s.adminViews(ctx, []ClubClaim{c})
	if err != nil {
		return ClaimView{}, err
	}
	v := views[0]
	v.Approver = ap.Approver
	if f := c.EvidenceFileID; f != nil {
		if s.d.Files == nil {
			return ClaimView{}, errUnavailable
		}
		dl, err := s.d.Files.PresignGet(ctx, *f, evidenceTTL)
		switch {
		case errors.Is(err, files.ErrNotClean), errors.Is(err, problem.ErrNotFound):
			v.EvidenceState = EvidenceNotClean
		case err != nil:
			return ClaimView{}, err
		default:
			v.Evidence, v.EvidenceState = &dl, EvidenceAvailable
		}
	}
	meta := map[string]any{"operation": "read", authz.AttrStatus: string(c.Status), "evidence": v.EvidenceState}
	if err := s.d.Audit.Log(ctx, claimAudit(a, authz.DirectoryClubClaimList, c, v.Club, false, meta)); err != nil {
		return ClaimView{}, err
	}
	return v, nil
}

// AdminDecideClubClaim implements Service. Order: authz on the gate (403
// before reading for everyone but platform_admin and university reps) →
// the body (422) → in one transaction: the claim locked (404) → its
// approver (E-21: the university's rep, else platform_admin) and authz on
// the real claim (a rep of another university or of a sensitive club 404,
// platform_admin where a rep approves 403; 03 Y6: a claimant never decides
// their own claim, 403, club_claims_reviewer_chk backs it) → pending only
// (409) → approval: the club locked, its org not closed (409), the
// claimant's active verification of the club's university (422 k8), then
// orgs.OrgAdmin.GrantRole(club_admin) (422
// orgs.club_admin_requires_adult_student: the whole decision rolls back),
// the claim, clubs.claim_status = claimed; rejection: the claim only → the
// audit record → club_claim.decided (O; the worker tells the claimant).
func (s *service) AdminDecideClubClaim(ctx context.Context, a authz.Actor, id uuid.UUID, in ClaimDecision) (ClaimView, error) {
	if err := authz.Can(ctx, a, authz.DirectoryClubClaimDecide, claimGate(id)); err != nil {
		return ClaimView{}, err
	}
	approve := in.Decision == ClaimApproved
	if !approve && in.Decision != ClaimRejected {
		return ClaimView{}, problem.Field("body.decision", "must be approved or rejected")
	}
	note, err := cleanMultiline("body.decision_note", in.Note, maxDecisionNote)
	if err != nil {
		return ClaimView{}, err
	}
	if approve && s.d.Students == nil {
		return ClaimView{}, errUnavailable
	}
	hash, err := idempotency.Hash(struct {
		Op       string
		Claim    uuid.UUID
		Decision ClaimState
		Note     string
	}{"decide_claim", id, in.Decision, deref(note)})
	if err != nil {
		return ClaimView{}, err
	}
	var ap approval
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		c, err := s.d.Store.LockClaim(ctx, id)
		if err != nil {
			return uuid.Nil(), err
		}
		club, err := s.d.Store.GetClub(ctx, c.ClubOrgID)
		if err != nil {
			return uuid.Nil(), err
		}
		if ap, err = s.approvalOf(ctx, club, c.ClaimantUserID, c.CreatedAt); err != nil {
			return uuid.Nil(), err
		}
		if err := authz.Can(ctx, a, authz.DirectoryClubClaimDecide, claimResource(c, ap)); err != nil {
			return uuid.Nil(), err
		}
		if c.Status != ClaimPending {
			return uuid.Nil(), ErrClaimState
		}
		if approve {
			if club, err = s.approve(ctx, c); err != nil {
				return uuid.Nil(), err
			}
		}
		now, by := s.now(), a.UserID
		c.Status, c.ReviewedBy, c.ReviewedAt, c.DecisionNote = in.Decision, &by, &now, note
		if c, err = s.d.Store.UpdateClaim(ctx, c); err != nil {
			return uuid.Nil(), err
		}
		if approve && club.ClaimStatus != ClaimClaimed {
			club.ClaimStatus = ClaimClaimed
			if _, err := s.d.Store.UpdateClub(ctx, club); err != nil {
				return uuid.Nil(), err
			}
		}
		meta := map[string]any{"decision": string(in.Decision), "decision_note": note != nil, "approver": ap.Approver}
		if approve {
			meta["granted_role"] = string(authz.MemberClubAdmin)
		}
		if err := s.d.Audit.Log(ctx, claimAudit(a, authz.DirectoryClubClaimDecide, c, club, approve, meta)); err != nil {
			return uuid.Nil(), err
		}
		env, err := domainevent.New(domainevent.ClubClaimDecided{
			ClaimID: c.ID, ClubOrgID: c.ClubOrgID, UserID: c.ClaimantUserID, Outcome: string(in.Decision),
		}, "club_claim", c.ID)
		if err != nil {
			return uuid.Nil(), err
		}
		return c.ID, s.d.Outbox.Add(ctx, env)
	})
	if err != nil {
		return ClaimView{}, err
	}
	c, err := s.d.Store.GetClaim(ctx, id)
	if err != nil {
		return ClaimView{}, err
	}
	views, err := s.adminViews(ctx, []ClubClaim{c})
	if err != nil {
		return ClaimView{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "club claim decided", slog.String("claim_id", c.ID.String()),
		slog.String("decision", string(c.Status)))
	v := views[0]
	v.Approver = ap.Approver
	return v, nil
}

// approve is the club side of an approval, in the decision's transaction:
// the club locked, its org writable (a closed club takes no admin), the
// claimant's verification still of the club's university (k8), then
// club_admin granted by orgs (01 §6.3). A failure rolls the decision back.
func (s *service) approve(ctx context.Context, c ClubClaim) (Club, error) {
	club, err := s.d.Store.LockClub(ctx, c.ClubOrgID)
	if err != nil {
		return Club{}, err
	}
	o, err := s.d.Orgs.Get(ctx, club.OrgID)
	if err != nil {
		return Club{}, err
	}
	if o.Status == orgs.StatusClosed {
		return Club{}, ErrInvalidState
	}
	v, ok, err := s.d.Students.Active(ctx, c.ClaimantUserID)
	switch {
	case err != nil:
		return Club{}, err
	case !ok:
		return Club{}, orgs.ErrClubAdminNeedsAdultStudent // the verification lapsed since the claim
	case v.UniversityID != club.UniversityID:
		return Club{}, ErrUniversityMismatch
	}
	err = s.d.Orgs.GrantRole(ctx, club.OrgID, c.ClaimantUserID, authz.MemberClubAdmin, "club_claim:"+c.ID.String())
	if err != nil {
		return Club{}, err
	}
	return club, nil
}
