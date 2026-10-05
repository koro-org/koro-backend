// Package students is the student profile module (01 §4.9; BE-023): the
// profile and its sections (skills, experiences, languages), the privacy
// settings, the audience keys (audience_keys, audience_eligible) that feed
// and outreach target, and the visibility DTOs of every level a company or
// the student sees (dto.go), the CVs, the audience counts and streams
// (Audience over count_segment), the club_admin's member card and the
// platform staff's admin view (BE-025). Level 3 access grants and the
// company endpoints come with BE-046, which serve the level 2 and 3
// projections built here.
//
// Rules the package enforces:
//
//   - Visibility levels (04 §V; agenda §4): a company sees level 0
//     (counts, k ≥ 10), level 1 (targeted delivery, no recipient list),
//     level 2 (the anonymous profile, only when the student turned
//     discoverable on) and level 3 (the full profile, only with an active
//     grant of that very company). Each level is a separate DTO type whose
//     fields are an allow-list (dto.go); the projections refuse a minor,
//     drop every sensitive club and answer ErrNotFound for another
//     company's grant (IDOR).
//   - Private by default (02 §9.2): discoverable (level 2), contact details
//     and club memberships at level 3 are off until the student turns them
//     on. discoverable and show_contact_on_grant change only together with
//     their consent record (discovery_profile, contact_info_share) in the
//     same transaction (T-33: explicit consent); every change is audited.
//   - Minors (03 k9, Y4): a user under 18 can never turn discoverability or
//     contact sharing on (422 students.minor_restricted), is never audience
//     eligible and never appears in a company view.
//   - The university comes from the active student verification and is
//     never taken from a request; the department must be an active one of
//     that university (directory.Catalog).
//   - Audience keys ('all', 'uni:<id>', 'dept:<id>', 'year:<n>',
//     'club:<id>'; 02 §9.1) hold non-sensitive clubs only (03 Y2, T-80):
//     when a club becomes sensitive its key leaves every profile
//     (club.sensitivity_changed), students.audience_refresh is the nightly
//     safety net. ViewerKeys, read by the feed only, adds the sensitive
//     clubs at read time and is never stored.
//   - k-anonymity (04 §V.2, 02 §9.1): Audience.Count answers through
//     count_segment and never returns a number below 10
//     (ErrAudienceTooSmall); Audience.Stream refuses such an audience and
//     walks only eligible adults. The dimension count is derived from the
//     keys (T-132) and sensitive club keys never target (03 Y2).
//   - CVs (02 §9.6): at most 3, one current, PDF uploads of the student
//     only (files: ClamAV, metadata removed), downloads by ≤ 5-minute
//     signed URL with an access audit record, hard delete with the file.
//     A CV or an avatar the scan rejects loses its reference and its file
//     (file.rejected, T-176).
//   - The member card (04 §V.4) has no e-mail (T-69) and exists for an
//     active member of the club_admin's own club only; a sensitive club's
//     card is audited. The admin view (04 §V.5) is break-glass for
//     support and platform_admin and request-bound for the dpo (03 k11);
//     every read is audited.
//   - Talent values never enter this module (01 §0-7): no DTO carries a
//     talent, band, score, point or rank field (TestDTO_SelfProfileHasNoTalentField,
//     TestDTO_NoLevelCarriesTalentField).
//   - Personal data: logs carry IDs only; the KVKK provider exports and
//     erases every row of the module, the avatar and the CV files.
package students

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
)

// AudienceProfile is the targeting view of a student (01 §4.9).
type AudienceProfile struct {
	UniversityID uuid.UUID
	DepartmentID *uuid.UUID
	ClassYear    *int16
	IsMinor      bool
	Eligible     bool
}

// ViewerKeys are the keys a feed reader matches posts with (01 §4.9): the
// profile keys plus every active club membership, sensitive clubs
// included, computed at read time and never stored or shown to anyone.
type ViewerKeys struct {
	Keys []string
}

// Cohort is a student's cohort for talent (01 §4.11).
type Cohort struct {
	UniversityID uuid.UUID
	ClassYear    *int16
	IsMinor      bool
}

// Profiles is the read contract of other modules (01 §4.9).
type Profiles interface {
	// AudienceProfile returns the targeting view of userID or ErrNotFound.
	AudienceProfile(ctx context.Context, userID uuid.UUID) (AudienceProfile, error)
	// ViewerKeys returns the feed keys of userID (feed reads only): 'all'
	// for everyone, the profile keys of a student, every active club.
	ViewerKeys(ctx context.Context, userID uuid.UUID) (ViewerKeys, error)
	// Cohorts returns the cohorts of the students among userIDs.
	Cohorts(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]Cohort, error)
	// RankingChoice returns the activity ranking choice of userID
	// (default without a settings row).
	RankingChoice(ctx context.Context, userID uuid.UUID) (RankingChoice, error)
}

// Service is the students service: the cross-module contracts plus the
// HTTP use cases (04 §2.4). Every method that takes an actor calls
// authz.Can first (05 §5.3).
type Service interface {
	Profiles
	Audience

	// GraduatedAmong reports which of userIDs have a graduated profile
	// (orgs.admin_sweep, T-210).
	GraduatedAmong(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]bool, error)
	// AvatarURLs returns short-lived avatar URLs of the users among
	// userIDs that have a clean avatar (orgs member lists, 04 §2.2).
	AvatarURLs(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error)

	// GetMyProfile is getMyStudentProfile (students.profile.read_own).
	GetMyProfile(ctx context.Context, a authz.Actor) (ProfileView, error)
	// UpdateMyProfile is updateMyStudentProfile (students.profile.update_own).
	UpdateMyProfile(ctx context.Context, a authz.Actor, p ProfilePatch) (ProfileView, error)
	// PutMySkills is putMySkills (students.profile_section.manage): the
	// list replaces the student's skills (≤ 30).
	PutMySkills(ctx context.Context, a authz.Actor, items []SkillInput) ([]Skill, error)
	// CreateMyExperience is createMyExperience (students.profile_section.manage).
	CreateMyExperience(ctx context.Context, a authz.Actor, in ExperienceInput) (Experience, error)
	// UpdateMyExperience is updateMyExperience: another user's entry is 404.
	UpdateMyExperience(ctx context.Context, a authz.Actor, id uuid.UUID, p ExperiencePatch) (Experience, error)
	// DeleteMyExperience is deleteMyExperience: another user's entry is 404.
	DeleteMyExperience(ctx context.Context, a authz.Actor, id uuid.UUID) error
	// PutMyLanguages is putMyLanguages (students.profile_section.manage).
	PutMyLanguages(ctx context.Context, a authz.Actor, items []LanguageInput) ([]Language, error)
	// GetMyPrivacy is getMyPrivacy (students.privacy.read_own).
	GetMyPrivacy(ctx context.Context, a authz.Actor) (Privacy, error)
	// UpdateMyPrivacy is updateMyPrivacy (students.privacy.update_own).
	UpdateMyPrivacy(ctx context.Context, a authz.Actor, p PrivacyPatch) (Privacy, error)

	// ListMyCVs is listMyCvs (students.cv.read_own).
	ListMyCVs(ctx context.Context, a authz.Actor) ([]CVView, error)
	// CreateMyCV is createMyCv (students.cv.manage): 409 students.cv_limit
	// for a fourth CV.
	CreateMyCV(ctx context.Context, a authz.Actor, fileID uuid.UUID, makeCurrent bool) (CVView, error)
	// SetMyCurrentCV is setMyCurrentCv (students.cv.manage).
	SetMyCurrentCV(ctx context.Context, a authz.Actor, id uuid.UUID) (CVView, error)
	// DeleteMyCV is deleteMyCv (students.cv.manage): the file goes too.
	DeleteMyCV(ctx context.Context, a authz.Actor, id uuid.UUID) error
	// DownloadMyCV is downloadMyCv (students.cv.read_own): ≤ 5 minutes.
	DownloadMyCV(ctx context.Context, a authz.Actor, id uuid.UUID) (files.Download, error)
	// GetMemberCard is getMemberCard (students.profile.read_member_card).
	GetMemberCard(ctx context.Context, a authz.Actor, orgID, membershipID uuid.UUID) (MemberCard, error)
	// AdminGetStudentProfile is adminGetStudentProfile
	// (students.profile.read_admin).
	AdminGetStudentProfile(ctx context.Context, a authz.Actor, userID uuid.UUID, access AdminAccess) (AdminView, error)
}

// Deps are the dependencies of the students module. Store, Tx, Outbox,
// Audit, Idempotency, Clock, Log, Consent, Catalog, Orgs and Users are
// required (New returns an error otherwise).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Outbox      outbox.Writer       // student.ranking_choice_changed, student.graduated (O); section completed (A)
	Audit       AuditLogger         // every write, same transaction
	Idempotency *idempotency.Keeper // optional Idempotency-Key of createMyExperience (06 H-5)
	Clock       clock.Clock
	Log         *slog.Logger

	Consent ConsentRecorder // discovery_profile, contact_info_share (same transaction)
	Catalog Catalog         // directory: names, department/skill validation, sensitive clubs
	Orgs    Memberships     // orgs: club memberships of the audience keys
	Users   Users           // identity: account status and age

	// Verifications is identity's student verification. nil → nobody has
	// an active verification: no profile is audience eligible and the
	// graduation check treats every candidate as not renewed.
	Verifications Verifications
	// Consents checks consents in force for the nightly reconciliation of
	// the mirrored flags. nil → the reconciliation is skipped.
	Consents ConsentChecker
	// Files attaches, serves and deletes the avatar and the CVs. nil →
	// avatar_file_id and createMyCv/downloadMyCv are refused (503), no
	// avatar_url is returned and a file to delete is kept (logged).
	Files Files

	// Members reads the membership behind a member card (orgs). nil →
	// getMemberCard answers 503 after its authz decision.
	Members Members
	// Accounts is identity's account view of the admin student view. nil
	// → adminGetStudentProfile answers 503 after its authz decision.
	Accounts AdminUsers
	// DSRs checks the dpo's data subject request (kvkk). nil → the dpo
	// path is closed (403; support and platform_admin keep break-glass).
	DSRs DSRs
	// Registrations returns the events reader, read at call time (events
	// builds later). nil, or a nil reader → the member card's count and
	// the admin view's registrations are absent.
	Registrations func() Registrations
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Outbox", d.Outbox != nil}, {"Audit", d.Audit != nil},
		{"Idempotency", d.Idempotency != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
		{"Consent", d.Consent != nil}, {"Catalog", d.Catalog != nil}, {"Orgs", d.Orgs != nil}, {"Users", d.Users != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("students.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what students hands to the composition root.
type Module struct {
	Service Service
	// Privacy is the KVKK provider (profile, sections, settings, CVs,
	// avatar and CV files).
	Privacy privacy.Provider

	svc *service
}

// New builds the students module; a missing dependency is a wiring bug
// that stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Privacy: privacyProvider{s: s}, svc: s}, nil
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
