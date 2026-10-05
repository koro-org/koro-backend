// Package directory is the reference data and club catalog module (01 §4.8;
// BE-020): universities, faculties, departments and the students' e-mail
// domains (YÖK public data, loaded by `koro seed reference`), the skill
// taxonomy, the club catalog (category, join policy, sensitive flag) and
// club claims (BE-022).
//
// Rules the package enforces:
//
//   - Reference data is public data only: names of universities, faculties
//     and departments and their .edu.tr domains. No personal data.
//   - A club or university org is opened with orgs.OrgAdmin.Create in
//     directory's own transaction (01 §6.3, T-78): when the directory row
//     fails, the org is rolled back with it.
//   - The sensitive flag (02 §8.5): religious, political and identity clubs
//     are always sensitive (CHECK; a club admin's change to such a category
//     is 422 directory.sensitivity_required, 03 k7). Only a platform admin
//     sets or clears the flag, with re-authentication; the change emits
//     club.sensitivity_changed (O) in the same transaction (T-80), whose
//     consumer (students, BE-023) drops the club's audience keys. A
//     sensitive club shows no member count and is never offered to company
//     targeting: Catalog.SensitiveOrgIDs is the filter outreach and
//     students apply (03 §0, Y2). The flag is never logged.
//   - Member counts are shown only from 10 members on (k ≥ 10) and never
//     for a sensitive club.
//   - Club claims (02 §8.6, 03 §5.3 k8): an adult student whose active
//     verification is of the club's university asks to run a club, with
//     evidence (text and/or a club_claim_evidence file attached in the same
//     transaction). A claimant has one pending claim per club
//     (club_claims_pending_uq); a club may get several club_admins (E-21),
//     so a claimed club takes claims too. The approver decides (E-21): the
//     university_rep of the club's university when it has one (other than
//     the claimant) and the club is not sensitive, a platform admin
//     otherwise; nobody decides their own claim (03 Y6,
//     club_claims_reviewer_chk). Approval is one transaction:
//     orgs.OrgAdmin.GrantRole(club_admin) (01 §6.3), the claim,
//     clubs.claim_status = claimed and club_claim.decided (O), whose worker
//     tells the claimant. The decision note is never shown to the claimant;
//     the review queue and every claim read are audited.
//   - Turkish search (02 §0.3, 05 §9.5): full text with the koro_tr
//     configuration (turkish stemmer), plus koro_norm (tr-TR lower case +
//     unaccent; İ/ı/I/i fold to i) substring and trigram matches; names
//     sort with the tr collation.
package directory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// Catalog is the read contract of other modules (01 §4.8).
type Catalog interface {
	// GetClub returns the catalog data of a club org or ErrNotFound.
	GetClub(ctx context.Context, orgID uuid.UUID) (Club, error)
	// SensitiveOrgIDs reports which of orgIDs are sensitive clubs (only
	// those keys are present, with true).
	SensitiveOrgIDs(ctx context.Context, orgIDs []uuid.UUID) (map[uuid.UUID]bool, error)
	// GetUniversity returns a university (any is_active) or ErrNotFound.
	GetUniversity(ctx context.Context, id uuid.UUID) (University, error)
	// UniversityByEmailDomain maps an e-mail domain to its university: the
	// domain equals a registered one or ends with "." + it; the longest
	// active match of an active university wins (02 §8.2).
	UniversityByEmailDomain(ctx context.Context, domain string) (uuid.UUID, bool, error)
	// GetDepartment returns a department or ErrNotFound.
	GetDepartment(ctx context.Context, id uuid.UUID) (Department, error)
	// SkillsExist returns ErrUnknownSkill unless every id is an active skill.
	SkillsExist(ctx context.Context, ids []uuid.UUID) error
}

// Service is the directory service: the cross-module contracts plus the
// HTTP use cases (04 §2.3). Every method that takes an actor calls
// authz.Can first (05 §5.3).
type Service interface {
	Catalog

	// UniversityOrgs returns the orgs of a university (its own org and its
	// clubs) for orgs searchOrgs?university_id (T-180).
	UniversityOrgs(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error)

	// ListUniversities is listUniversities (directory.university.list, P).
	ListUniversities(ctx context.Context, a authz.Actor, q UniversityQuery) (UniversityPage, error)
	// GetUniversityView is getUniversity (directory.university.read, P):
	// the university with its active faculties.
	GetUniversityView(ctx context.Context, a authz.Actor, id uuid.UUID) (UniversityView, error)
	// ListDepartments is listDepartments (directory.department.list, P).
	ListDepartments(ctx context.Context, a authz.Actor, universityID uuid.UUID, q DepartmentQuery) (DepartmentPage, error)
	// ListSkills is listSkills (directory.skill.list, P).
	ListSkills(ctx context.Context, a authz.Actor, q SkillQuery) (SkillPage, error)
	// ListClubs is listClubs (directory.club.list, P).
	ListClubs(ctx context.Context, a authz.Actor, q ClubQuery) (ClubPage, error)
	// GetClubView is getClub (directory.club.read, P).
	GetClubView(ctx context.Context, a authz.Actor, clubID uuid.UUID) (ClubView, error)
	// UpdateClub is updateClub (directory.club.update): ifMatch is the
	// If-Match header (04 §1.13: missing → 428, stale → 412).
	UpdateClub(ctx context.Context, a authz.Actor, clubID uuid.UUID, ifMatch string, p ClubPatch) (ClubView, error)
	// RequestClubSensitivity is requestClubSensitivity
	// (directory.club.request_sensitivity): audited, the platform admins get
	// an in-app notice (T-67: no table).
	RequestClubSensitivity(ctx context.Context, a authz.Actor, clubID uuid.UUID, reason SensitiveReason, note string) error
	// AdminCreateClub is adminCreateClub (directory.club.create): the club's
	// org and its clubs row in one transaction.
	AdminCreateClub(ctx context.Context, a authz.Actor, in ClubInput) (ClubView, error)
	// AdminSetClubSensitivity is adminSetClubSensitivity
	// (directory.club.set_sensitivity, R): emits club.sensitivity_changed in
	// the same transaction when the flag changes.
	AdminSetClubSensitivity(ctx context.Context, a authz.Actor, clubID uuid.UUID, in SensitivityInput, proof identity.ReauthProof) (ClubView, error)
	// AdminCreateUniversity is adminCreateUniversity (directory.university.manage).
	AdminCreateUniversity(ctx context.Context, a authz.Actor, in UniversityInput) (University, error)
	// AdminUpdateUniversity is adminUpdateUniversity (directory.university.manage).
	AdminUpdateUniversity(ctx context.Context, a authz.Actor, id uuid.UUID, p UniversityPatch) (University, error)
	// AdminPutUniversityEmailDomain is adminPutUniversityEmailDomain
	// (directory.university.manage); created reports a new domain.
	AdminPutUniversityEmailDomain(ctx context.Context, a authz.Actor, universityID uuid.UUID, domain string, in DomainInput) (EmailDomain, bool, error)
	// AdminCreateDepartment is adminCreateDepartment (directory.university.manage).
	AdminCreateDepartment(ctx context.Context, a authz.Actor, universityID uuid.UUID, in DepartmentInput) (Department, error)
	// AdminCreateSkill is adminCreateSkill (directory.skill.manage).
	AdminCreateSkill(ctx context.Context, a authz.Actor, in SkillInput) (Skill, error)
	// AdminUpdateSkill is adminUpdateSkill (directory.skill.manage).
	AdminUpdateSkill(ctx context.Context, a authz.Actor, id uuid.UUID, p SkillPatch) (Skill, error)

	// CreateClubClaim is createClubClaim (directory.club_claim.create, k8).
	CreateClubClaim(ctx context.Context, a authz.Actor, clubID uuid.UUID, in ClaimInput) (ClaimView, error)
	// ListMyClubClaims is listMyClubClaims (directory.club_claim.read_own).
	ListMyClubClaims(ctx context.Context, a authz.Actor, after *uuid.UUID, limit int) (ClaimPage, error)
	// WithdrawMyClubClaim is withdrawMyClubClaim (directory.club_claim.withdraw):
	// pending → withdrawn.
	WithdrawMyClubClaim(ctx context.Context, a authz.Actor, id uuid.UUID) (ClaimView, error)
	// AdminListClubClaims is adminListClubClaims (directory.club_claim.list):
	// the review queue of one status (default pending), audited.
	AdminListClubClaims(ctx context.Context, a authz.Actor, q AdminClaimQuery) (ClaimPage, error)
	// AdminGetClubClaim is adminGetClubClaim (directory.club_claim.list):
	// the claim with a short-lived evidence download, audited.
	AdminGetClubClaim(ctx context.Context, a authz.Actor, id uuid.UUID) (ClaimView, error)
	// AdminDecideClubClaim is adminDecideClubClaim
	// (directory.club_claim.decide): approval grants club_admin, marks the
	// club claimed and emits club_claim.decided in one transaction.
	AdminDecideClubClaim(ctx context.Context, a authz.Actor, id uuid.UUID, in ClaimDecision) (ClaimView, error)

	// ImportReference upserts the public reference data (`koro seed
	// reference`; 02 §23: a command, not a migration). It is idempotent:
	// a second run with the same data changes nothing.
	ImportReference(ctx context.Context, data ReferenceData) (ImportResult, error)
}

// Deps are the dependencies of the directory module. Store, Tx, Outbox,
// Queue, Audit, Orgs, Idempotency, Clock and Log are required (New returns
// an error otherwise).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Outbox      outbox.Writer       // club.sensitivity_changed (O)
	Queue       queue.Enqueuer      // the sensitivity request notice task (A)
	Audit       AuditLogger         // every write, same transaction
	Orgs        Orgs                // club/university orgs (01 §6.3) and public profiles
	Idempotency *idempotency.Keeper // optional Idempotency-Key of the POST operations (06 H-5)
	Clock       clock.Clock
	Log         *slog.Logger

	// Reauth verifies the proof of adminSetClubSensitivity (identity). nil
	// → it fails closed with 503.
	Reauth Reauthenticator
	// MemberCount returns a club's active member count (orgs, BE-019). nil
	// → member_count is null.
	MemberCount MemberCounter
	// Sender and PlatformAdmins serve the sensitivity request notice in the
	// worker (Sender also the claim decision notice). nil → the task fails
	// (and is retried) instead of dropping it.
	Sender         Sender
	PlatformAdmins PlatformAdmins

	// Files serves the claim evidence (files, L2) and deletes it (erasure,
	// file.rejected, retention). nil → a claim with an evidence file and the
	// admin's download fail closed with 503; evidence to delete stays
	// (logged).
	Files Files
	// Students is identity's student verification: a claim approval checks
	// the claimant's university again. nil → approval fails closed (503).
	Students Students
	// Users names the claimants in the admin review (optional).
	Users Users
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Outbox", d.Outbox != nil}, {"Queue", d.Queue != nil},
		{"Audit", d.Audit != nil}, {"Orgs", d.Orgs != nil}, {"Idempotency", d.Idempotency != nil},
		{"Clock", d.Clock != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("directory.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what directory hands to the composition root.
type Module struct {
	Service Service
	// Privacy is the KVKK provider of directory (club claims and their
	// evidence; 01 §3.3).
	Privacy privacy.Provider
	// Sweeper ends the retention of decided club claims and their evidence
	// (02 §24.2 club_claim; BE-057).
	Sweeper privacy.RetentionSweeper
	svc     *service
}

// New builds the directory module; a missing dependency is a wiring bug
// that stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Privacy: privacyProvider{s: s}, Sweeper: sweeper{s: s}, svc: s}, nil
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
