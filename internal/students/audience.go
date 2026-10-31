package students

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/civil"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Worker and job names (01 §6.2, §7; <module>.<verb>_<object>).
const (
	// AddClubKeyTaskName consumes membership.started: a club's key joins
	// the profile unless the club is sensitive.
	AddClubKeyTaskName = "students.add_club_key"
	// RemoveClubKeyTaskName consumes membership.ended.
	RemoveClubKeyTaskName = "students.remove_club_key"
	// ApplyEduVerifiedTaskName consumes user.edu_verified: the profile is
	// created (or follows the verified university) and is recomputed.
	ApplyEduVerifiedTaskName = "students.apply_edu_verified"
	// ApplyEduLapsedTaskName consumes user.edu_verification_lapsed: the
	// profile leaves the audience.
	ApplyEduLapsedTaskName = "students.apply_edu_lapsed"
	// DropSensitiveClubKeyTaskName consumes club.sensitivity_changed
	// (T-80): a club that became sensitive leaves every profile's keys; a
	// club that stopped being sensitive comes back to its members' keys.
	DropSensitiveClubKeyTaskName = "students.drop_sensitive_club_key"
	// AudienceRefreshJobName recomputes every profile's audience columns
	// and reconciles the consent-mirrored privacy flags (01 §7).
	AudienceRefreshJobName = "students.audience_refresh" // 02:00, low
	// GraduationCheckJobName graduates the profiles whose expected
	// graduation month is over and whose verification was not renewed
	// since (01 §7) → student.graduated.
	GraduationCheckJobName = "students.graduation_check" // 02:15, default

	// Audit actions of the system paths (03 §4.1 format; not HTTP actions).
	auditPrivacyReconcile  = "students.privacy.reconcile"
	auditProfileGraduate   = "students.profile.graduate"
	auditProfileUniversity = "students.profile.follow_verification"

	batchSize = 1000 // rows per transaction (01 §7)
)

// Audience key prefixes (02 §9.1). 'all' is a dimension of its own.
const (
	KeyAll  = "all"
	keyUni  = "uni:"
	keyDept = "dept:"
	keyYear = "year:"
	keyClub = "club:"
)

// ClubKey is the audience key of a club org.
func ClubKey(orgID uuid.UUID) string { return keyClub + orgID.String() }

// facts are what other modules know about a student and the audience
// depends on.
type facts struct {
	verification *identity.EduVerification // active one; nil: none
	user         *identity.UserSummary     // nil: unknown account
	clubs        []uuid.UUID               // active club memberships, sensitive clubs included
}

// adult reports a known account that is not a minor (an unknown account
// counts as a minor: fail closed).
func (f facts) adult() bool { return f.user != nil && !f.user.IsMinor }

// profileKeys are the keys of the profile itself (no clubs).
func profileKeys(p Profile) []string {
	keys := []string{KeyAll, keyUni + p.UniversityID.String()}
	if p.DepartmentID != nil {
		keys = append(keys, keyDept+p.DepartmentID.String())
	}
	if p.ClassYear != nil {
		keys = append(keys, keyYear+strconv.Itoa(int(*p.ClassYear)))
	}
	return keys
}

// sortKeys sorts and de-duplicates keys (a stable stored order: drift
// detection compares slices).
func sortKeys(keys []string) []string {
	slices.Sort(keys)
	return slices.Compact(keys)
}

// audienceOf returns the stored audience of p (02 §9.1): the profile keys
// plus the keys of its NON-sensitive clubs (03 Y2), and eligibility =
// active verification ∧ adult ∧ profile active ∧ account active.
func audienceOf(p Profile, f facts, sensitive map[uuid.UUID]bool) ([]string, bool) {
	keys := profileKeys(p)
	for _, c := range f.clubs {
		if !sensitive[c] {
			keys = append(keys, ClubKey(c))
		}
	}
	eligible := f.verification != nil && f.adult() && f.user.Status == identity.StatusActive &&
		p.Status == ProfileActive
	return sortKeys(keys), eligible
}

// reconcile returns p following its facts: the university of the active
// verification (the department then goes: it belongs to the old one) and
// the audience columns. at stamps audience_refreshed_at when they change.
func reconcile(p Profile, f facts, sensitive map[uuid.UUID]bool, at time.Time) Profile {
	next := cloneProfile(p)
	if v := f.verification; v != nil && v.UniversityID != next.UniversityID {
		next.UniversityID, next.DepartmentID = v.UniversityID, nil
		next.CompletedSections = next.withSection(SectionBasics, false)
	}
	keys, eligible := audienceOf(next, f, sensitive)
	if !slices.Equal(keys, []string(next.AudienceKeys)) || eligible != next.AudienceEligible || next.AudienceRefreshedAt == nil {
		next.AudienceKeys, next.AudienceEligible, next.AudienceRefreshedAt = keys, eligible, &at
	}
	return next
}

// changed reports whether reconcile changed anything worth a write.
func changed(old, next Profile) bool {
	return old.UniversityID != next.UniversityID || !equalID(old.DepartmentID, next.DepartmentID) ||
		old.AudienceEligible != next.AudienceEligible || !slices.Equal(old.AudienceKeys, next.AudienceKeys) ||
		(old.AudienceRefreshedAt == nil) != (next.AudienceRefreshedAt == nil)
}

func equalID(a, b *uuid.UUID) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// loadFacts reads the facts of ids from identity and orgs and the
// sensitive flag of their clubs from directory.
func (s *service) loadFacts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]facts, map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]facts, len(ids))
	if len(ids) == 0 {
		return out, map[uuid.UUID]bool{}, nil
	}
	users, err := s.d.Users.GetMany(ctx, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("students: users: %w", err)
	}
	ver := map[uuid.UUID]identity.EduVerification{}
	if s.d.Verifications != nil {
		if ver, err = s.d.Verifications.ActiveMany(ctx, ids); err != nil {
			return nil, nil, fmt.Errorf("students: verifications: %w", err)
		}
	}
	var clubs []uuid.UUID
	for _, id := range ids {
		f := facts{}
		if u, ok := users[id]; ok {
			f.user = &u
		}
		if v, ok := ver[id]; ok {
			f.verification = &v
		}
		if f.clubs, err = s.clubsOf(ctx, id); err != nil {
			return nil, nil, err
		}
		clubs = append(clubs, f.clubs...)
		out[id] = f
	}
	sensitive := map[uuid.UUID]bool{}
	if len(clubs) > 0 {
		slices.SortFunc(clubs, compareUUID)
		if sensitive, err = s.d.Catalog.SensitiveOrgIDs(ctx, slices.Compact(clubs)); err != nil {
			return nil, nil, fmt.Errorf("students: sensitive clubs: %w", err)
		}
	}
	return out, sensitive, nil
}

// clubsOf returns the clubs userID is an active member of (any role).
func (s *service) clubsOf(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	ms, err := s.d.Orgs.MembershipsOf(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("students: memberships: %w", err)
	}
	var out []uuid.UUID
	for _, m := range ms {
		if m.OrgKind == authz.OrgClub && !slices.Contains(out, m.OrgID) {
			out = append(out, m.OrgID)
		}
	}
	return out, nil
}

// withAudience returns p reconciled with its current facts (HTTP writes).
func (s *service) withAudience(ctx context.Context, p Profile) (Profile, error) {
	fs, sensitive, err := s.loadFacts(ctx, []uuid.UUID{p.UserID})
	if err != nil {
		return Profile{}, err
	}
	return reconcile(p, fs[p.UserID], sensitive, s.now()), nil
}

// refreshUser recomputes the audience of userID's profile in a
// transaction; a user without a profile is nobody's audience yet.
func (s *service) refreshUser(ctx context.Context, userID uuid.UUID, job string) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.profiles().LockProfile(ctx, userID)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		}
		return s.applyReconcile(ctx, p, job)
	})
}

// applyReconcile writes the reconciled p (locked in the transaction
// carried by ctx) when anything changed; a university change is audited.
func (s *service) applyReconcile(ctx context.Context, p Profile, job string) error {
	next, err := s.withAudience(ctx, p)
	if err != nil {
		return err
	}
	if !changed(p, next) {
		return nil
	}
	if _, err := s.profiles().UpdateProfile(ctx, next); err != nil {
		return err
	}
	if next.UniversityID != p.UniversityID {
		return s.d.Audit.Log(ctx, systemEvent(job, auditProfileUniversity, p.UserID, map[string]any{
			"university_id": next.UniversityID.String(), "department_cleared": p.DepartmentID != nil,
		}))
	}
	return nil
}

// systemEvent is the audit record of a system write on a student's data.
func systemEvent(job, action string, userID uuid.UUID, meta map[string]any) audit.Event {
	id := userID
	if meta == nil {
		meta = map[string]any{}
	}
	meta[audit.MetaSubjectUserID] = userID.String()
	return audit.Event{Action: action, Outcome: audit.Success, ResourceType: resourceProfile, ResourceID: &id,
		Metadata: meta}.By(authz.SystemActor(job))
}

// ---- Profiles (01 §4.9)

// AudienceProfile implements Profiles. An unknown account counts as a
// minor (fail closed).
func (s *service) AudienceProfile(ctx context.Context, userID uuid.UUID) (AudienceProfile, error) {
	p, err := s.profiles().GetProfile(ctx, userID)
	if err != nil {
		return AudienceProfile{}, err
	}
	users, err := s.d.Users.GetMany(ctx, []uuid.UUID{userID})
	if err != nil {
		return AudienceProfile{}, err
	}
	u, ok := users[userID]
	return AudienceProfile{
		UniversityID: p.UniversityID, DepartmentID: p.DepartmentID, ClassYear: p.ClassYear,
		IsMinor: !ok || u.IsMinor, Eligible: p.AudienceEligible && ok && !u.IsMinor,
	}, nil
}

// ViewerKeys implements Profiles: 'all', the profile keys of a student
// (whatever the eligibility: reading is not targeting) and the keys of
// every active club membership, sensitive clubs included (a club's own
// posts reach its members; 02 §17). Computed at read time, never stored.
func (s *service) ViewerKeys(ctx context.Context, userID uuid.UUID) (ViewerKeys, error) {
	keys := []string{KeyAll}
	switch p, err := s.profiles().GetProfile(ctx, userID); {
	case err == nil:
		keys = profileKeys(p)
	case !errors.Is(err, ErrNotFound):
		return ViewerKeys{}, err
	}
	clubs, err := s.clubsOf(ctx, userID)
	if err != nil {
		return ViewerKeys{}, err
	}
	for _, c := range clubs {
		keys = append(keys, ClubKey(c))
	}
	return ViewerKeys{Keys: sortKeys(keys)}, nil
}

// Cohorts implements Profiles.
func (s *service) Cohorts(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]Cohort, error) {
	out := map[uuid.UUID]Cohort{}
	rows, err := s.profiles().Profiles(ctx, userIDs)
	if err != nil || len(rows) == 0 {
		return out, err
	}
	ids := make([]uuid.UUID, len(rows))
	for i, p := range rows {
		ids[i] = p.UserID
	}
	users, err := s.d.Users.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		u, ok := users[p.UserID]
		out[p.UserID] = Cohort{UniversityID: p.UniversityID, ClassYear: p.ClassYear, IsMinor: !ok || u.IsMinor}
	}
	return out, nil
}

// RankingChoice implements Profiles.
func (s *service) RankingChoice(ctx context.Context, userID uuid.UUID) (RankingChoice, error) {
	p, err := s.privacy().GetPrivacy(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return RankingDefault, nil
	case err != nil:
		return "", err
	}
	return p.ActivityRankingChoice, nil
}

// GraduatedAmong implements Service.
func (s *service) GraduatedAmong(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	ids, err := s.profiles().GraduatedAmong(ctx, userIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// ---- event consumers (01 §6.2)

// Subscribe registers the outbox subscriptions of students (01 §6.2).
func (m Module) Subscribe(r *outbox.Registry) {
	q := outbox.Queue(queue.Default)
	r.Subscribe(domainevent.TopicMembershipStarted, AddClubKeyTaskName, m.svc.handleMembershipStarted, q)
	r.Subscribe(domainevent.TopicMembershipEnded, RemoveClubKeyTaskName, m.svc.handleMembershipEnded, q)
	r.Subscribe(domainevent.TopicUserEduVerified, ApplyEduVerifiedTaskName, m.svc.handleEduVerified, q)
	r.Subscribe(domainevent.TopicUserEduVerificationLapsed, ApplyEduLapsedTaskName, m.svc.handleEduLapsed, q)
	r.Subscribe(domainevent.TopicClubSensitivityChanged, DropSensitiveClubKeyTaskName, m.svc.handleSensitivityChanged, q)
	files.SubscribeRejected(r, studentsModule, DropRejectedFileTaskName, m.svc.dropRejectedFile)
	r.Subscribe(domainevent.TopicUserEduVerificationLapsed, SuspendLapsedGrantsTaskName, m.svc.handleLapsedGrants, q)
	r.Subscribe(domainevent.TopicUserEduVerified, SuspendGapGrantsTaskName, m.svc.handleGapGrants, q)
	r.Subscribe(domainevent.TopicConsentRecorded, ClearContactSharesTaskName, m.svc.handleConsentRecorded, q)
}

// Jobs returns the scheduled jobs of 01 §7.
func (m Module) Jobs() []queue.Job {
	return []queue.Job{
		{Name: AudienceRefreshJobName, Cron: "0 2 * * *", Queue: queue.Low, Unique: 23 * time.Hour,
			Handle: m.svc.audienceRefresh},
		{Name: GraduationCheckJobName, Cron: "15 2 * * *", Queue: queue.Default, Unique: 23 * time.Hour,
			Handle: m.svc.graduationCheck},
		{Name: ExpireSuspendedGrantsJobName, Cron: "30 2 * * *", Queue: queue.Default, Unique: 23 * time.Hour,
			Handle: m.svc.expireSuspendedGrants},
	}
}

// handleMembershipStarted is students.add_club_key (membership.started):
// the student's keys are recomputed (a sensitive club stays out).
func (s *service) handleMembershipStarted(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.MembershipStarted](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if authz.OrgKind(p.OrgKind) != authz.OrgClub {
		return nil
	}
	return s.refreshUser(ctx, p.UserID, AddClubKeyTaskName)
}

// handleMembershipEnded is students.remove_club_key (membership.ended).
// Recomputing from the active memberships keeps a key while another active
// membership of the same club remains (a role change).
func (s *service) handleMembershipEnded(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.MembershipEnded](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if authz.OrgKind(p.OrgKind) != authz.OrgClub {
		return nil
	}
	return s.refreshUser(ctx, p.UserID, RemoveClubKeyTaskName)
}

// handleEduVerified is students.apply_edu_verified (user.edu_verified): a
// verified student without a profile gets one (the university of the
// verification, degree level bachelor until the student picks a
// department), so the targeting reaches them; a graduated profile whose
// student verified again is active again (renewed after graduation,
// T-56); then the audience is recomputed. Idempotent.
func (s *service) handleEduVerified(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.UserEduVerified](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.profiles().LockProfile(ctx, p.UserID)
		switch {
		case errors.Is(err, ErrNotFound):
			fresh := Profile{UserID: p.UserID, UniversityID: p.UniversityID, DegreeLevel: DegreeBachelor, Status: ProfileActive}
			if _, _, err := s.profiles().InsertProfile(ctx, fresh); err != nil {
				return err
			}
			if cur, err = s.profiles().LockProfile(ctx, p.UserID); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		if cur.Status == ProfileGraduated {
			cur.Status, cur.GraduatedAt = ProfileActive, nil
			if cur, err = s.profiles().UpdateProfile(ctx, cur); err != nil {
				return err
			}
			if err := s.d.Audit.Log(ctx, systemEvent(ApplyEduVerifiedTaskName, auditProfileGraduate, p.UserID,
				map[string]any{"status": string(ProfileActive), "reason": "verification_renewed"})); err != nil {
				return err
			}
		}
		return s.applyReconcile(ctx, cur, ApplyEduVerifiedTaskName)
	})
}

// handleEduLapsed is students.apply_edu_lapsed
// (user.edu_verification_lapsed): without an active verification the
// profile is no longer audience eligible (a renewal in between keeps it).
func (s *service) handleEduLapsed(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.UserEduVerificationLapsed](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.refreshUser(ctx, p.UserID, ApplyEduLapsedTaskName)
}

// handleSensitivityChanged is students.drop_sensitive_club_key
// (club.sensitivity_changed, T-80). Sensitive: the club's key leaves every
// profile in batches (special-category data must not target, 03 Y2).
// Cleared: the club's active members are recomputed and get the key back.
// Idempotent.
func (s *service) handleSensitivityChanged(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ClubSensitivityChanged](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.Sensitive {
		key := ClubKey(p.ClubOrgID)
		total, err := db.Drain(ctx, batchSize, func(ctx context.Context) (int, error) {
			return s.profiles().RemoveAudienceKey(ctx, key, s.now(), batchSize)
		})
		if err != nil {
			return fmt.Errorf("%s: %w", DropSensitiveClubKeyTaskName, err)
		}
		if total > 0 {
			s.d.Log.LogAttrs(ctx, slog.LevelInfo, "sensitive club key removed from audiences",
				slog.String("org_id", p.ClubOrgID.String()), slog.Int("profiles", total))
		}
		return nil
	}
	members, err := s.d.Orgs.ActiveMemberIDs(ctx, p.ClubOrgID)
	if err != nil {
		return fmt.Errorf("%s: %w", DropSensitiveClubKeyTaskName, err)
	}
	for _, u := range members {
		if err := s.refreshUser(ctx, u, DropSensitiveClubKeyTaskName); err != nil {
			return fmt.Errorf("%s: %w", DropSensitiveClubKeyTaskName, err)
		}
	}
	return nil
}

// ---- nightly jobs (01 §7)

// audienceRefresh is students.audience_refresh: every profile is
// recomputed from its current facts in batches (a student who turned 18, a
// lapsed verification, a missed event, a club that became sensitive), and
// the consent-mirrored privacy flags are reconciled.
func (s *service) audienceRefresh(ctx context.Context) error {
	after, fixed := uuid.Nil(), 0
	for ctx.Err() == nil {
		rows, err := s.profiles().ProfilesAfter(ctx, after, batchSize)
		if err != nil {
			return fmt.Errorf("%s: %w", AudienceRefreshJobName, err)
		}
		if len(rows) == 0 {
			break
		}
		after = rows[len(rows)-1].UserID
		n, err := s.refreshBatch(ctx, rows)
		if err != nil {
			return fmt.Errorf("%s: %w", AudienceRefreshJobName, err)
		}
		fixed += n
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if fixed > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "student audiences refreshed", slog.Int("changed", fixed))
	}
	return s.reconcilePrivacy(ctx)
}

// refreshBatch recomputes rows in one transaction and returns how many
// changed. The facts are read once for the batch; each row is locked and
// reconciled from its fresh state (a concurrent write is never undone).
func (s *service) refreshBatch(ctx context.Context, rows []Profile) (int, error) {
	ids := make([]uuid.UUID, len(rows))
	for i, p := range rows {
		ids[i] = p.UserID
	}
	fs, sensitive, err := s.loadFacts(ctx, ids)
	if err != nil {
		return 0, err
	}
	n := 0
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		now := s.now()
		for _, id := range ids {
			cur, err := s.profiles().LockProfile(ctx, id)
			switch {
			case errors.Is(err, ErrNotFound):
				continue // erased meanwhile
			case err != nil:
				return err
			}
			next := reconcile(cur, fs[id], sensitive, now)
			if !changed(cur, next) {
				continue
			}
			if _, err := s.profiles().UpdateProfile(ctx, next); err != nil {
				return err
			}
			if next.UniversityID != cur.UniversityID {
				if err := s.d.Audit.Log(ctx, systemEvent(AudienceRefreshJobName, auditProfileUniversity, id, map[string]any{
					"university_id": next.UniversityID.String(), "department_cleared": cur.DepartmentID != nil,
				})); err != nil {
					return err
				}
			}
			n++
		}
		return nil
	})
	return n, err
}

// reconcilePrivacy turns off a mirrored flag whose consent is no longer in
// force (withdrawn through the consent ledger, or the purpose switched
// off) and every flag of a minor (02 §9.2: never true for minors). Each
// change is audited. Turning show_contact_on_grant off also stops every
// contact share of the student, as the student's own switch does (BE-104:
// the backstop of a lost consent.recorded). Without a consent checker it
// does nothing.
func (s *service) reconcilePrivacy(ctx context.Context) error {
	if s.d.Consents == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "privacy reconciliation skipped: no consent checker")
		return nil
	}
	after, fixed := uuid.Nil(), 0
	for ctx.Err() == nil {
		rows, err := s.privacy().PrivacyFlagged(ctx, after, batchSize)
		if err != nil {
			return fmt.Errorf("%s: %w", AudienceRefreshJobName, err)
		}
		if len(rows) == 0 {
			break
		}
		after = rows[len(rows)-1].UserID
		n, err := s.reconcilePrivacyBatch(ctx, rows)
		if err != nil {
			return fmt.Errorf("%s: %w", AudienceRefreshJobName, err)
		}
		fixed += n
	}
	if fixed > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "privacy flags reconciled with consents", slog.Int("changed", fixed))
	}
	return ctx.Err()
}

func (s *service) reconcilePrivacyBatch(ctx context.Context, rows []Privacy) (int, error) {
	ids := make([]uuid.UUID, len(rows))
	for i, p := range rows {
		ids[i] = p.UserID
	}
	disc, err := s.d.Consents.FilterActive(ctx, ids, consent.PurposeDiscoveryProfile)
	if err != nil {
		return 0, err
	}
	contact, err := s.d.Consents.FilterActive(ctx, ids, consent.PurposeContactInfoShare)
	if err != nil {
		return 0, err
	}
	users, err := s.d.Users.GetMany(ctx, ids)
	if err != nil {
		return 0, err
	}
	n := 0
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		for _, id := range ids {
			u, known := users[id]
			minor := !known || u.IsMinor
			cur, err := s.privacy().LockPrivacy(ctx, id)
			if err != nil {
				return err
			}
			next := cur
			next.Discoverable = cur.Discoverable && slices.Contains(disc, id) && !minor
			next.ShowContactOnGrant = cur.ShowContactOnGrant && slices.Contains(contact, id) && !minor
			if next == cur {
				continue
			}
			if _, err := s.privacy().UpdatePrivacy(ctx, next); err != nil {
				return err
			}
			reason := "consent_not_in_force"
			if minor {
				reason = "minor"
			}
			meta := map[string]any{"reason": reason}
			if next.Discoverable != cur.Discoverable {
				meta["discoverable"] = false
			}
			if next.ShowContactOnGrant != cur.ShowContactOnGrant {
				meta["show_contact_on_grant"] = false
			}
			if err := s.d.Audit.Log(ctx, systemEvent(AudienceRefreshJobName, auditPrivacyReconcile, id, meta)); err != nil {
				return err
			}
			if next.ShowContactOnGrant != cur.ShowContactOnGrant {
				// Like the student turning it off: no share outlives the
				// consent (the backstop of students.clear_contact_shares; BE-104).
				gs, err := s.grants().ClearGrantContacts(ctx, id, nil)
				if err != nil {
					return err
				}
				if err := s.auditUnshares(ctx, authz.SystemActor(AudienceRefreshJobName), gs, reason); err != nil {
					return err
				}
			}
			n++
		}
		return nil
	})
	return n, err
}

// graduationCheck is students.graduation_check: an active profile whose
// expected graduation month is over (expected_graduation before the first
// day of the current month, Europe/Istanbul) and whose student
// verification was not renewed since that month becomes graduated
// (graduated_at, out of the audience) and student.graduated (O) is written
// in the same transaction (orgs ends the club_admin roles).
func (s *service) graduationCheck(ctx context.Context) error {
	now := s.now()
	today := civil.Today(now)
	cutoff, err := civil.New(today.Year, today.Month, 1)
	if err != nil {
		return err
	}
	after, graduated := uuid.Nil(), 0
	for ctx.Err() == nil {
		rows, err := s.profiles().GraduationCandidates(ctx, cutoff, after, batchSize)
		if err != nil {
			return fmt.Errorf("%s: %w", GraduationCheckJobName, err)
		}
		if len(rows) == 0 {
			break
		}
		after = rows[len(rows)-1].UserID
		n, err := s.graduateBatch(ctx, rows, cutoff, now)
		if err != nil {
			return fmt.Errorf("%s: %w", GraduationCheckJobName, err)
		}
		graduated += n
	}
	if graduated > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "student profiles graduated", slog.Int("graduated", graduated))
	}
	return ctx.Err()
}

// renewedSince reports whether v was verified in or after the month of
// graduation (a student who renewed is still studying).
func renewedSince(v identity.EduVerification, graduation civil.Date) bool {
	return !civil.Today(v.VerifiedAt).Before(graduation)
}

func (s *service) graduateBatch(ctx context.Context, rows []Profile, cutoff civil.Date, now time.Time) (int, error) {
	ids := make([]uuid.UUID, len(rows))
	for i, p := range rows {
		ids[i] = p.UserID
	}
	ver := map[uuid.UUID]identity.EduVerification{}
	if s.d.Verifications != nil {
		var err error
		if ver, err = s.d.Verifications.ActiveMany(ctx, ids); err != nil {
			return 0, err
		}
	}
	n := 0
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		for _, id := range ids {
			cur, err := s.profiles().LockProfile(ctx, id)
			switch {
			case errors.Is(err, ErrNotFound):
				continue
			case err != nil:
				return err
			}
			if cur.Status != ProfileActive || cur.ExpectedGraduation == nil || !cur.ExpectedGraduation.Before(cutoff) {
				continue // changed meanwhile
			}
			if v, ok := ver[id]; ok && renewedSince(v, *cur.ExpectedGraduation) {
				continue
			}
			at := now
			cur.Status, cur.GraduatedAt = ProfileGraduated, &at
			cur.AudienceEligible, cur.AudienceRefreshedAt = false, &at
			if _, err := s.profiles().UpdateProfile(ctx, cur); err != nil {
				return err
			}
			if err := s.emitOutbox(ctx, domainevent.StudentGraduated{UserID: id}, "student_profile", id); err != nil {
				return err
			}
			if err := s.d.Audit.Log(ctx, systemEvent(GraduationCheckJobName, auditProfileGraduate, id,
				map[string]any{"status": string(ProfileGraduated)})); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}
