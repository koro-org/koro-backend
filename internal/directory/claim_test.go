package directory_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
)

// verifications is identity's student verification (orgs and directory).
type verifications struct {
	mu sync.Mutex
	m  map[uuid.UUID]identity.EduVerification
}

func (v *verifications) set(user, uni uuid.UUID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.m[user] = identity.EduVerification{UniversityID: uni, VerifiedAt: t0, ExpiresAt: t0.AddDate(1, 0, 0), Method: "email"}
}

func (v *verifications) drop(user uuid.UUID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.m, user)
}

func (v *verifications) Active(_ context.Context, user uuid.UUID) (identity.EduVerification, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ev, ok := v.m[user]
	return ev, ok, nil
}

func (v *verifications) ActiveMany(ctx context.Context, users []uuid.UUID) (map[uuid.UUID]identity.EduVerification, error) {
	out := map[uuid.UUID]identity.EduVerification{}
	for _, u := range users {
		if ev, ok, _ := v.Active(ctx, u); ok {
			out[u] = ev
		}
	}
	return out, nil
}

// userDir is identity's user directory (orgs and directory).
type userDir struct {
	mu sync.Mutex
	m  map[uuid.UUID]identity.UserSummary
}

func (d *userDir) set(u identity.UserSummary) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.m[u.ID] = u
}

func (d *userDir) Get(_ context.Context, id uuid.UUID) (identity.UserSummary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	u, ok := d.m[id]
	if !ok {
		return identity.UserSummary{}, identity.ErrNotFound
	}
	return u, nil
}

func (d *userDir) GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.UserSummary, error) {
	out := map[uuid.UUID]identity.UserSummary{}
	for _, id := range ids {
		if u, err := d.Get(ctx, id); err == nil {
			out[id] = u
		}
	}
	return out, nil
}

func (d *userDir) Contact(ctx context.Context, id uuid.UUID) (identity.UserContact, error) {
	u, err := d.Get(ctx, id)
	if err != nil {
		return identity.UserContact{}, err
	}
	return identity.UserContact{ID: u.ID, Email: "u@example.test", FirstName: u.FirstName, LastName: u.LastName}, nil
}

func (*userDir) FindByEmail(context.Context, string) (uuid.UUID, bool, error) {
	return uuid.Nil(), false, nil
}

// fakeFiles is files for claim evidence; Delete records into removed.
type fakeFiles struct {
	mu       sync.Mutex
	m        map[uuid.UUID]files.FileMeta
	attached []files.Owner
	removed  *removedFiles
}

func (f *fakeFiles) Delete(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	f.mu.Lock()
	delete(f.m, id)
	f.mu.Unlock()
	return f.removed.remove(ctx, a, id)
}

func (f *fakeFiles) put(m files.FileMeta) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[m.ID] = m
}

func (f *fakeFiles) Get(_ context.Context, id uuid.UUID) (files.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.m[id]
	if !ok {
		return files.FileMeta{}, problem.ErrNotFound
	}
	return m, nil
}

func (f *fakeFiles) Attach(_ context.Context, _ authz.Actor, id uuid.UUID, o files.Owner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.m[id]
	switch {
	case !ok:
		return problem.ErrNotFound
	case m.AttachedModule != "":
		return files.ErrAlreadyAttached
	}
	m.AttachedModule, m.AttachedEntityID = o.Module, o.EntityID
	f.m[id] = m
	f.attached = append(f.attached, o)
	return nil
}

func (f *fakeFiles) PresignGet(_ context.Context, id uuid.UUID, ttl time.Duration) (files.Download, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.m[id]
	switch {
	case !ok:
		return files.Download{}, problem.ErrNotFound
	case m.ScanStatus != files.ScanClean:
		return files.Download{}, files.ErrNotClean
	}
	return files.Download{URL: "https://files.example.test/" + id.String(), ExpiresAt: t0.Add(ttl)}, nil
}

// removedFiles records the deleted files.
type removedFiles struct {
	mu  sync.Mutex
	ids []uuid.UUID
}

func (r *removedFiles) remove(_ context.Context, _ authz.Actor, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return nil
}

// GrantRole records the transaction of the grant (and can fail it).
func (s *spyOrgs) GrantRole(ctx context.Context, orgID, userID uuid.UUID, role authz.MemberRole, reason string) error {
	s.mu.Lock()
	s.grants = append(s.grants, txOf(ctx))
	fail := s.failGrant
	s.mu.Unlock()
	if fail != nil {
		return fail
	}
	return s.Service.GrantRole(ctx, orgID, userID, role, reason)
}

// UpdateClaim records the transaction of each claim write.
func (s *spyStore) UpdateClaim(ctx context.Context, c directory.ClubClaim) (directory.ClubClaim, error) {
	s.mu.Lock()
	s.claimUpdates = append(s.claimUpdates, txOf(ctx))
	s.mu.Unlock()
	return s.Store.UpdateClaim(ctx, c)
}

// claimWorld is a university with a club and a sensitive club.
type claimWorld struct {
	*env
	uni   directory.University
	club  directory.ClubView
	sens  directory.ClubView
	other directory.University
}

func newClaimWorld(t *testing.T) claimWorld {
	t.Helper()
	e := newEnv(t)
	w := claimWorld{env: e, uni: e.university(t, "C-1", "Talep Üniversitesi", "Bursa")}
	w.other = e.university(t, "C-2", "Öteki Üniversite", "Ankara")
	w.club = e.club(t, w.uni, "Satranç Kulübü", directory.CategoryHobby)
	w.sens = e.club(t, w.uni, "İnanç Kulübü", directory.CategoryReligious)
	return w
}

// student is a verified student of uni (an account in identity, a
// verification in identity), as the API would resolve them.
func (w claimWorld) student(t *testing.T, uni directory.University, minor bool) authz.Actor {
	t.Helper()
	id := uuid.NewV7()
	w.ver.set(id, uni.ID)
	w.users.set(identity.UserSummary{ID: id, Status: identity.StatusActive, FirstName: "Ece", LastName: "Kaya", IsMinor: minor})
	return authz.Actor{Kind: authz.KindUser, UserID: id, SessionID: uuid.NewV7(), StudentStatus: authz.StudentVerified,
		UniversityID: new(uni.ID), IsMinor: minor}
}

// rep is an active university_rep of uni (an org_members row and the
// Actor's membership), with an MFA session.
func (w claimWorld) rep(t *testing.T, uni directory.University) authz.Actor {
	t.Helper()
	id := uuid.NewV7()
	w.users.set(identity.UserSummary{ID: id, Status: identity.StatusActive, FirstName: "Temsil", LastName: "Ci"})
	if _, err := w.orgsMem.InsertMember(t.Context(), orgs.Membership{ID: uuid.NewV7(), OrgID: uni.OrgID, OrgKind: orgs.KindUniversity,
		UserID: id, Role: authz.MemberUniversityRep, Status: orgs.MemberActive, Source: orgs.SourceAdmin, JoinedAt: new(t0)}); err != nil {
		t.Fatal(err)
	}
	return authz.Actor{Kind: authz.KindUser, UserID: id, SessionID: uuid.NewV7(), MFAVerified: true,
		Memberships: []authz.Membership{{OrgID: uni.OrgID, OrgKind: authz.OrgUniversity, Role: authz.MemberUniversityRep}}}
}

func (w claimWorld) claim(t *testing.T, a authz.Actor, club uuid.UUID) directory.ClubClaim {
	t.Helper()
	v, err := w.svc.CreateClubClaim(t.Context(), a, club, directory.ClaimInput{RoleInClub: "Başkan", EvidenceText: "Tutanak ekte."})
	if err != nil {
		t.Fatalf("CreateClubClaim: %v", err)
	}
	return v.Claim
}

func approveIn() directory.ClaimDecision {
	return directory.ClaimDecision{Decision: directory.ClaimApproved}
}

func (w claimWorld) isAdmin(t *testing.T, club, user uuid.UUID) bool {
	t.Helper()
	ok, err := w.orgs.HasRole(t.Context(), club, user, authz.MemberClubAdmin)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestCreateClaim_Minor_403(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	_, err := w.svc.CreateClubClaim(t.Context(), w.student(t, w.uni, true), w.club.Club.OrgID,
		directory.ClaimInput{RoleInClub: "Başkan", EvidenceText: "Tutanak"})
	wantCode(t, err, 403, "directory.minor_restricted")
	// An account without a student verification: H-52, the orgs value.
	unverified := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), SessionID: uuid.NewV7()}
	_, err = w.svc.CreateClubClaim(t.Context(), unverified, w.club.Club.OrgID, directory.ClaimInput{RoleInClub: "Başkan", EvidenceText: "x"})
	wantErr(t, err, orgs.ErrVerificationRequired)
	wantCode(t, err, 403, "students.verification_required")
	if len(w.audit.of(authz.DirectoryClubClaimCreate)) != 0 {
		t.Fatal("a refused claim was audited as created")
	}
}

func TestCreateClaim_UniversityMismatch_422(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	_, err := w.svc.CreateClubClaim(t.Context(), w.student(t, w.other, false), w.club.Club.OrgID,
		directory.ClaimInput{RoleInClub: "Başkan", EvidenceText: "Tutanak"})
	wantCode(t, err, 422, "directory.university_mismatch")
	// Unknown club 404; no evidence 422; a club admin of the club 409.
	s := w.student(t, w.uni, false)
	_, err = w.svc.CreateClubClaim(t.Context(), s, uuid.NewV7(), directory.ClaimInput{RoleInClub: "Başkan", EvidenceText: "x"})
	wantErr(t, err, problem.ErrNotFound)
	_, err = w.svc.CreateClubClaim(t.Context(), s, w.club.Club.OrgID, directory.ClaimInput{RoleInClub: "Başkan", EvidenceText: " \n "})
	wantCode(t, err, 422, "validation_failed")
	admin := s
	admin.Memberships = []authz.Membership{{OrgID: w.club.Club.OrgID, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}
	_, err = w.svc.CreateClubClaim(t.Context(), admin, w.club.Club.OrgID, directory.ClaimInput{RoleInClub: "Başkan", EvidenceText: "x"})
	wantCode(t, err, 409, "directory.invalid_state")
}

func TestCreateClaim_DuplicatePending_409(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	s := w.student(t, w.uni, false)
	first := w.claim(t, s, w.club.Club.OrgID)
	if first.Status != directory.ClaimPending || first.ClaimantUserID != s.UserID || first.ClubOrgID != w.club.Club.OrgID {
		t.Fatalf("claim = %+v", first)
	}
	_, err := w.svc.CreateClubClaim(t.Context(), s, w.club.Club.OrgID, directory.ClaimInput{RoleInClub: "YK üyesi", EvidenceText: "x"})
	wantCode(t, err, 409, "directory.claim_pending")
	// Another club is fine; after a withdrawal the same club is too.
	w.claim(t, s, w.sens.Club.OrgID)
	if _, err := w.svc.WithdrawMyClubClaim(t.Context(), s, first.ID); err != nil {
		t.Fatal(err)
	}
	w.claim(t, s, w.club.Club.OrgID)
	// The claim is the claimant's record: audited without org scope.
	evs := w.audit.of(authz.DirectoryClubClaimCreate)
	if len(evs) != 3 || evs[0].OrgID != nil || evs[0].Metadata[audit.MetaSubjectUserID] != s.UserID.String() {
		t.Fatalf("audit = %+v", evs)
	}
	if evs[1].Metadata[audit.MetaSensitive] != true {
		t.Fatalf("a sensitive club's claim is not marked: %+v", evs[1].Metadata)
	}
}

func TestCreateClaim_EvidenceFile(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	s := w.student(t, w.uni, false)
	file := func(purpose files.Purpose, by uuid.UUID, scan files.ScanStatus) uuid.UUID {
		id := uuid.NewV7()
		w.files.put(files.FileMeta{ID: id, Purpose: purpose, UploadedBy: &by, Status: files.StatusActive, ScanStatus: scan})
		return id
	}
	for name, id := range map[string]uuid.UUID{
		"unknown":       uuid.NewV7(),
		"another's":     file(files.PurposeClubClaimEvidence, uuid.NewV7(), files.ScanClean),
		"wrong purpose": file(files.PurposeAvatar, s.UserID, files.ScanClean),
		"infected":      file(files.PurposeClubClaimEvidence, s.UserID, files.ScanInfected),
	} {
		_, err := w.svc.CreateClubClaim(t.Context(), s, w.club.Club.OrgID, directory.ClaimInput{RoleInClub: "Başkan", EvidenceFileID: &id})
		if !isStatus(err, 422) {
			t.Fatalf("%s file: %v", name, err)
		}
	}
	good := file(files.PurposeClubClaimEvidence, s.UserID, files.ScanPending)
	v, err := w.svc.CreateClubClaim(t.Context(), s, w.club.Club.OrgID, directory.ClaimInput{RoleInClub: "Başkan", EvidenceFileID: &good})
	if err != nil || v.Claim.EvidenceFileID == nil || *v.Claim.EvidenceFileID != good || v.Claim.EvidenceText != nil {
		t.Fatalf("claim with a file = %+v, %v", v.Claim, err)
	}
	if len(w.files.attached) != 1 || w.files.attached[0] != (files.Owner{Module: "directory", EntityID: v.Claim.ID,
		Purpose: files.PurposeClubClaimEvidence}) {
		t.Fatalf("attached = %+v", w.files.attached)
	}
	// The scan has not cleared the file: the reviewer sees not_clean, no URL.
	got, err := w.svc.AdminGetClubClaim(t.Context(), pad(t), v.Claim.ID)
	if err != nil || got.EvidenceState != directory.EvidenceNotClean || got.Evidence != nil {
		t.Fatalf("admin view = %+v, %v", got, err)
	}
	m, _ := w.files.Get(t.Context(), good)
	m.ScanStatus = files.ScanClean
	w.files.put(m)
	got, err = w.svc.AdminGetClubClaim(t.Context(), pad(t), v.Claim.ID)
	if err != nil || got.EvidenceState != directory.EvidenceAvailable || got.Evidence == nil || !got.Evidence.ExpiresAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("admin view of a clean file = %+v, %v", got, err)
	}
	// The same upload cannot back a second claim.
	other := w.student(t, w.uni, false)
	_, err = w.svc.CreateClubClaim(t.Context(), other, w.sens.Club.OrgID, directory.ClaimInput{RoleInClub: "Başkan", EvidenceFileID: &good})
	if !isStatus(err, 422) {
		t.Fatalf("reused file: %v", err)
	}
}

func TestWithdrawClaim_OnlyPending(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	s := w.student(t, w.uni, false)
	c := w.claim(t, s, w.club.Club.OrgID)
	// Another user's claim is invisible (404), the platform admin's too.
	_, err := w.svc.WithdrawMyClubClaim(t.Context(), w.student(t, w.uni, false), c.ID)
	wantErr(t, err, problem.ErrNotFound)
	_, err = w.svc.WithdrawMyClubClaim(t.Context(), pad(t), c.ID)
	wantErr(t, err, problem.ErrNotFound)
	v, err := w.svc.WithdrawMyClubClaim(t.Context(), s, c.ID)
	if err != nil || v.Claim.Status != directory.ClaimWithdrawn || v.ClubName != "Satranç Kulübü" {
		t.Fatalf("withdraw = %+v, %v", v, err)
	}
	_, err = w.svc.WithdrawMyClubClaim(t.Context(), s, c.ID)
	wantCode(t, err, 409, "directory.claim_state")
	_, err = w.svc.AdminDecideClubClaim(t.Context(), pad(t), c.ID, approveIn())
	wantCode(t, err, 409, "directory.claim_state")
	// A decided claim cannot be withdrawn either.
	d := w.claim(t, s, w.sens.Club.OrgID)
	if _, err := w.svc.AdminDecideClubClaim(t.Context(), pad(t), d.ID,
		directory.ClaimDecision{Decision: directory.ClaimRejected, Note: "Kanıt yetersiz"}); err != nil {
		t.Fatal(err)
	}
	_, err = w.svc.WithdrawMyClubClaim(t.Context(), s, d.ID)
	wantCode(t, err, 409, "directory.claim_state")
	if n := len(w.audit.of(authz.DirectoryClubClaimWithdraw)); n != 1 {
		t.Fatalf("withdraw audits = %d", n)
	}
	// The claimant's list: newest first, without the decision note.
	page, err := w.svc.ListMyClubClaims(t.Context(), s, nil, 10)
	if err != nil || len(page.Items) != 2 || page.Items[0].Claim.ID != d.ID || page.Items[1].Claim.ID != c.ID {
		t.Fatalf("my claims = %+v, %v", page, err)
	}
}

func TestDecideClaim_ApproveGrantsClubAdminInSameTx(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	s := w.student(t, w.uni, false)
	c := w.claim(t, s, w.club.Club.OrgID) // the university has no rep: a platform admin decides
	v, err := w.svc.AdminDecideClubClaim(t.Context(), pad(t), c.ID, approveIn())
	if err != nil {
		t.Fatal(err)
	}
	if v.Claim.Status != directory.ClaimApproved || v.Claim.ReviewedBy == nil || *v.Claim.ReviewedBy != pad(t).UserID ||
		v.Claim.ReviewedAt == nil || v.Approver != authz.ClaimApproverPlatformAdmin {
		t.Fatalf("decided = %+v", v)
	}
	// One transaction: the grant, the claim and the club.
	w.orgs.mu.Lock()
	grants := slices.Clone(w.orgs.grants)
	w.orgs.mu.Unlock()
	w.store.mu.Lock()
	updates := slices.Clone(w.store.claimUpdates)
	w.store.mu.Unlock()
	if len(grants) != 1 || len(updates) != 1 || grants[0] == 0 || grants[0] != updates[0] {
		t.Fatalf("grant tx %v, claim tx %v: not one transaction", grants, updates)
	}
	if !w.isAdmin(t, w.club.Club.OrgID, s.UserID) {
		t.Fatal("claimant is not club_admin")
	}
	club, _ := w.mem.GetClub(t.Context(), w.club.Club.OrgID)
	if club.ClaimStatus != directory.ClaimClaimed {
		t.Fatalf("claim_status = %s", club.ClaimStatus)
	}
	evs := w.out.Events()
	var decided []domainevent.Envelope
	for _, ev := range evs {
		if ev.Topic == domainevent.TopicClubClaimDecided {
			decided = append(decided, ev)
		}
	}
	if len(decided) != 1 {
		t.Fatalf("club_claim.decided events = %d", len(decided))
	}
	p, err := domainevent.Decode[domainevent.ClubClaimDecided](decided[0])
	if err != nil || p.ClaimID != c.ID || p.UserID != s.UserID || p.Outcome != "approved" || p.ClubOrgID != w.club.Club.OrgID {
		t.Fatalf("payload = %+v, %v", p, err)
	}
	// The approval is org-scoped in the audit (the club's admins see who became an admin).
	au := w.audit.of(authz.DirectoryClubClaimDecide)
	if len(au) != 1 || au[0].OrgID == nil || *au[0].OrgID != w.club.Club.OrgID || au[0].Metadata["granted_role"] != "club_admin" {
		t.Fatalf("decide audit = %+v", au)
	}
	// The worker tells the claimant (once per claim; the note never goes out).
	r := outbox.NewRegistry()
	w.mod.Subscribe(r)
	subs := r.For(domainevent.TopicClubClaimDecided)
	if len(subs) != 1 {
		t.Fatalf("subscriptions = %d", len(subs))
	}
	b, _ := json.Marshal(decided[0])
	if err := subs[0].HandleTask(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if len(w.sender.msgs) != 1 || *w.sender.msgs[0].UserID != s.UserID || w.sender.msgs[0].Template != "directory.claim_approved" ||
		w.sender.msgs[0].Channel != notify.ChannelInApp || w.sender.msgs[0].DedupeKey != "directory.claim_decided:"+c.ID.String() {
		t.Fatalf("notice = %+v", w.sender.msgs)
	}
	// A second claimant of the claimed club becomes a second admin (E-21).
	s2 := w.student(t, w.uni, false)
	c2 := w.claim(t, s2, w.club.Club.OrgID)
	if _, err := w.svc.AdminDecideClubClaim(t.Context(), pad(t), c2.ID, approveIn()); err != nil || !w.isAdmin(t, w.club.Club.OrgID, s2.UserID) {
		t.Fatalf("second admin: %v", err)
	}
}

// TestDecideClaim_GrantFailure_LeavesClaimPending: a grant orgs refuses
// fails the decision before the claim or the club is written (the
// PostgreSQL rollback is TestDecideClaim_GrantFailure_RollsBackClaim).
func TestDecideClaim_GrantFailure_LeavesClaimPending(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	s := w.student(t, w.uni, false)
	c := w.claim(t, s, w.club.Club.OrgID)
	// The claimant turned out to be a minor in identity: orgs refuses.
	w.users.set(identity.UserSummary{ID: s.UserID, Status: identity.StatusActive, IsMinor: true})
	_, err := w.svc.AdminDecideClubClaim(t.Context(), pad(t), c.ID, approveIn())
	wantCode(t, err, 422, "orgs.club_admin_requires_adult_student")
	// The verification lapsed since the claim.
	w.users.set(identity.UserSummary{ID: s.UserID, Status: identity.StatusActive})
	w.ver.drop(s.UserID)
	_, err = w.svc.AdminDecideClubClaim(t.Context(), pad(t), c.ID, approveIn())
	wantCode(t, err, 422, "orgs.club_admin_requires_adult_student")
	// The verification moved to another university.
	w.ver.set(s.UserID, w.other.ID)
	_, err = w.svc.AdminDecideClubClaim(t.Context(), pad(t), c.ID, approveIn())
	wantCode(t, err, 422, "directory.university_mismatch")
	got, _ := w.mem.GetClaim(t.Context(), c.ID)
	club, _ := w.mem.GetClub(t.Context(), w.club.Club.OrgID)
	if got.Status != directory.ClaimPending || got.ReviewedBy != nil || club.ClaimStatus != directory.ClaimUnclaimed ||
		len(w.out.Topics()) != 0 || len(w.audit.of(authz.DirectoryClubClaimDecide)) != 0 {
		t.Fatalf("after failures: claim %+v, club %s, events %v", got, club.ClaimStatus, w.out.Topics())
	}
	// A rejection needs no grant.
	if _, err := w.svc.AdminDecideClubClaim(t.Context(), pad(t), c.ID, directory.ClaimDecision{Decision: directory.ClaimRejected}); err != nil {
		t.Fatal(err)
	}
}

func TestDecideClaim_ReviewerIsClaimant_Rejected(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	// A platform admin who is also a verified student of the university claims a club (no rep).
	admin := w.student(t, w.uni, false)
	admin.PlatformRoles, admin.MFAVerified = []authz.PlatformRole{authz.PlatformAdmin}, true
	c := w.claim(t, admin, w.club.Club.OrgID)
	_, err := w.svc.AdminDecideClubClaim(t.Context(), admin, c.ID, approveIn())
	wantErr(t, err, authz.ErrForbidden)
	// A university rep who claims a club of their own university: being the
	// claimant, they do not count as its approver (a platform admin decides)
	// and they cannot decide it themselves.
	rep := w.rep(t, w.uni)
	w.ver.set(rep.UserID, w.uni.ID)
	repStudent := rep
	repStudent.StudentStatus, repStudent.UniversityID = authz.StudentVerified, new(w.uni.ID)
	rc := w.claim(t, repStudent, w.sens.Club.OrgID) // sensitive: platform admin anyway
	_, err = w.svc.AdminDecideClubClaim(t.Context(), repStudent, rc.ID, approveIn())
	wantErr(t, err, problem.ErrNotFound)
	// On a non-sensitive club of their university the only rep is the
	// claimant: a platform admin approves, the rep cannot (Y6).
	rc2 := w.claim(t, repStudent, w.club.Club.OrgID)
	_, err = w.svc.AdminDecideClubClaim(t.Context(), repStudent, rc2.ID, approveIn())
	wantErr(t, err, problem.ErrNotFound)
	if v, err := w.svc.AdminGetClubClaim(t.Context(), pad(t), rc2.ID); err != nil || v.Approver != authz.ClaimApproverPlatformAdmin {
		t.Fatalf("approver of the only rep's own claim = %+v, %v", v.Approver, err)
	}
	// The DB CHECK backs it: the store refuses the claimant as reviewer.
	got, _ := w.mem.GetClaim(t.Context(), c.ID)
	now := t0
	got.Status, got.ReviewedBy, got.ReviewedAt = directory.ClaimApproved, &admin.UserID, &now
	if _, err := w.mem.UpdateClaim(t.Context(), got); !errors.Is(err, problem.ErrForbidden) {
		t.Fatalf("store accepted the claimant as reviewer: %v", err)
	}
	if got, _ := w.mem.GetClaim(t.Context(), c.ID); got.Status != directory.ClaimPending || w.isAdmin(t, w.club.Club.OrgID, admin.UserID) {
		t.Fatalf("claim %s after a self decision", got.Status)
	}
}

// TestDecideClaim_UniversityRepApproves pins E-21 (orchestrator,
// 2026-10-04): the club's university representative decides when the
// university has one; a platform admin only otherwise (or for a sensitive
// club, whose claim stays with platform staff).
func TestDecideClaim_UniversityRepApproves(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	rep := w.rep(t, w.uni)
	otherRep := w.rep(t, w.other)
	s := w.student(t, w.uni, false)
	c := w.claim(t, s, w.club.Club.OrgID)
	// A platform admin may not decide where a rep does (403); another
	// university's rep does not see the claim (404); support never decides.
	_, err := w.svc.AdminDecideClubClaim(t.Context(), pad(t), c.ID, approveIn())
	wantErr(t, err, authz.ErrForbidden)
	_, err = w.svc.AdminDecideClubClaim(t.Context(), otherRep, c.ID, approveIn())
	wantErr(t, err, problem.ErrNotFound)
	_, err = w.svc.AdminDecideClubClaim(t.Context(), persona(t, "support"), c.ID, approveIn())
	wantErr(t, err, authz.ErrForbidden)
	// A rep without an MFA session is asked for it.
	noMFA := rep
	noMFA.MFAVerified = false
	_, err = w.svc.AdminDecideClubClaim(t.Context(), noMFA, c.ID, approveIn())
	wantErr(t, err, authz.ErrMFARequired)
	// The rep reads it (with the approver) and approves it.
	got, err := w.svc.AdminGetClubClaim(t.Context(), rep, c.ID)
	if err != nil || got.Approver != authz.ClaimApproverRep {
		t.Fatalf("rep view = %+v, %v", got, err)
	}
	if _, err := w.svc.AdminGetClubClaim(t.Context(), otherRep, c.ID); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("another university's rep reads the claim: %v", err)
	}
	v, err := w.svc.AdminDecideClubClaim(t.Context(), rep, c.ID, approveIn())
	if err != nil || v.Claim.Status != directory.ClaimApproved || *v.Claim.ReviewedBy != rep.UserID || !w.isAdmin(t, w.club.Club.OrgID, s.UserID) {
		t.Fatalf("rep decision = %+v, %v", v.Claim, err)
	}
	if au := w.audit.of(authz.DirectoryClubClaimDecide); len(au) != 1 || au[0].Metadata["approver"] != authz.ClaimApproverRep {
		t.Fatalf("decide audit = %+v", au)
	}
	// A sensitive club's claim stays with platform staff: the rep does not see it.
	sc := w.claim(t, s, w.sens.Club.OrgID)
	_, err = w.svc.AdminDecideClubClaim(t.Context(), rep, sc.ID, approveIn())
	wantErr(t, err, problem.ErrNotFound)
	if _, err := w.svc.AdminGetClubClaim(t.Context(), rep, sc.ID); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("rep reads a sensitive club's claim: %v", err)
	}
	if _, err := w.svc.AdminDecideClubClaim(t.Context(), pad(t), sc.ID, approveIn()); err != nil {
		t.Fatalf("platform admin on a sensitive club: %v", err)
	}
	// An unknown claim: 404 for the rep and the platform admin; 403 for
	// everyone without a decide path, before anything is read.
	_, err = w.svc.AdminDecideClubClaim(t.Context(), rep, uuid.NewV7(), approveIn())
	wantErr(t, err, problem.ErrNotFound)
	_, err = w.svc.AdminDecideClubClaim(t.Context(), s, uuid.NewV7(), approveIn())
	wantErr(t, err, authz.ErrForbidden)
}

func TestAdminListClaims_RepScope(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	rep := w.rep(t, w.uni)
	otherClub := w.env.club(t, w.other, "Öteki Kulüp", directory.CategoryHobby)
	a := w.claim(t, w.student(t, w.uni, false), w.club.Club.OrgID)
	w.claim(t, w.student(t, w.uni, false), w.sens.Club.OrgID)
	w.claim(t, w.student(t, w.other, false), otherClub.Club.OrgID)
	ids := func(p directory.ClaimPage) []uuid.UUID {
		out := make([]uuid.UUID, len(p.Items))
		for i, v := range p.Items {
			out[i] = v.Claim.ID
		}
		return out
	}
	p, err := w.svc.AdminListClubClaims(t.Context(), rep, directory.AdminClaimQuery{})
	if err != nil || !slices.Equal(ids(p), []uuid.UUID{a.ID}) {
		t.Fatalf("rep queue = %v, %v", ids(p), err)
	}
	for _, staff := range []string{"support", "platform_admin"} {
		if p, err := w.svc.AdminListClubClaims(t.Context(), persona(t, staff), directory.AdminClaimQuery{}); err != nil || len(p.Items) != 3 {
			t.Fatalf("%s queue = %d, %v", staff, len(p.Items), err)
		}
	}
	noMFA := rep
	noMFA.MFAVerified = false
	_, err = w.svc.AdminListClubClaims(t.Context(), noMFA, directory.AdminClaimQuery{})
	wantErr(t, err, authz.ErrMFARequired)
	for _, other := range []authz.Actor{w.student(t, w.uni, false), persona(t, "moderator")} {
		_, err = w.svc.AdminListClubClaims(t.Context(), other, directory.AdminClaimQuery{})
		wantErr(t, err, authz.ErrForbidden)
	}
	_, err = w.svc.AdminListClubClaims(t.Context(), pad(t), directory.AdminClaimQuery{Status: "lost"})
	wantCode(t, err, 422, "validation_failed")
	// Every queue read is audited (claim IDs; sensitive when a sensitive club's claim is in it).
	au := w.audit.of(authz.DirectoryClubClaimList)
	if len(au) != 3 || au[0].Metadata["scope"] != authz.ClaimApproverRep || au[0].Metadata[audit.MetaSensitive] != nil ||
		au[1].Metadata[audit.MetaSensitive] != true {
		t.Fatalf("queue audits = %+v", au)
	}
}

func TestAdminGetClaim_Audited(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	s := w.student(t, w.uni, false)
	c := w.claim(t, s, w.sens.Club.OrgID)
	for _, staff := range []string{"support", "platform_admin"} {
		v, err := w.svc.AdminGetClubClaim(t.Context(), persona(t, staff), c.ID)
		if err != nil || v.Claim.ID != c.ID || v.Claimant == nil || v.Claimant.FirstName != "Ece" ||
			v.EvidenceState != directory.EvidenceNone || !v.Club.IsSensitive || v.Approver != authz.ClaimApproverPlatformAdmin {
			t.Fatalf("%s view = %+v, %v", staff, v, err)
		}
	}
	au := w.audit.of(authz.DirectoryClubClaimList)
	if len(au) != 2 {
		t.Fatalf("read audits = %d, want 2", len(au))
	}
	for _, e := range au {
		if e.ResourceID == nil || *e.ResourceID != c.ID || e.Metadata["operation"] != "read" || e.OrgID != nil ||
			e.Metadata[audit.MetaSubjectUserID] != s.UserID.String() || e.Metadata[audit.MetaSensitive] != true {
			t.Fatalf("read audit = %+v", e)
		}
	}
	// No read without the role (403 before reading), none of an unknown claim (404); nothing audited.
	_, err := w.svc.AdminGetClubClaim(t.Context(), s, c.ID)
	wantErr(t, err, authz.ErrForbidden)
	_, err = w.svc.AdminGetClubClaim(t.Context(), persona(t, "moderator"), c.ID)
	wantErr(t, err, authz.ErrForbidden)
	_, err = w.svc.AdminGetClubClaim(t.Context(), pad(t), uuid.NewV7())
	wantErr(t, err, problem.ErrNotFound)
	// A failing audit write withholds the view.
	w.audit.setFail(errors.New("audit down"))
	if _, err := w.svc.AdminGetClubClaim(t.Context(), pad(t), c.ID); err == nil {
		t.Fatal("view handed out without its audit record")
	}
	w.audit.setFail(nil)
	if n := len(w.audit.of(authz.DirectoryClubClaimList)); n != 2 {
		t.Fatalf("read audits = %d after the refusals", n)
	}
}

func TestClaimPrivacy_ExportErase(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	s := w.student(t, w.uni, false)
	fid := uuid.NewV7()
	w.files.put(files.FileMeta{ID: fid, Purpose: files.PurposeClubClaimEvidence, UploadedBy: &s.UserID, Status: files.StatusActive,
		ScanStatus: files.ScanClean})
	c1, err := w.svc.CreateClubClaim(t.Context(), s, w.club.Club.OrgID, directory.ClaimInput{RoleInClub: "Başkan", EvidenceFileID: &fid})
	if err != nil {
		t.Fatal(err)
	}
	c2 := w.claim(t, s, w.sens.Club.OrgID)
	if _, err := w.svc.AdminDecideClubClaim(t.Context(), pad(t), c2.ID,
		directory.ClaimDecision{Decision: directory.ClaimRejected, Note: "Kanıt yetersiz"}); err != nil {
		t.Fatal(err)
	}
	other := w.claim(t, w.student(t, w.uni, false), w.club.Club.OrgID)
	sec, err := w.mod.Privacy.Export(t.Context(), s.UserID)
	if err != nil || sec.Module != "directory" || len(sec.Items) != 2 {
		t.Fatalf("export = %+v, %v", sec, err)
	}
	byID := map[string]map[string]any{}
	for _, it := range sec.Items {
		byID[it["claim_id"].(string)] = it
	}
	if byID[c1.Claim.ID.String()]["evidence_file_id"] != fid.String() || byID[c2.ID.String()]["decision_note"] != "Kanıt yetersiz" {
		t.Fatalf("export items = %+v", sec.Items)
	}
	res, err := w.mod.Privacy.Erase(t.Context(), s.UserID)
	if err != nil || res.Deleted != 2 {
		t.Fatalf("erase = %+v, %v", res, err)
	}
	if !slices.Equal(w.removed.ids, []uuid.UUID{fid}) {
		t.Fatalf("removed files = %v", w.removed.ids)
	}
	for _, id := range []uuid.UUID{c1.Claim.ID, c2.ID} {
		if _, err := w.mem.GetClaim(t.Context(), id); !errors.Is(err, problem.ErrNotFound) {
			t.Fatalf("claim %v survived the erasure: %v", id, err)
		}
	}
	if _, err := w.mem.GetClaim(t.Context(), other.ID); err != nil {
		t.Fatalf("another user's claim was erased: %v", err)
	}
	// Idempotent.
	if res, err := w.mod.Privacy.Erase(t.Context(), s.UserID); err != nil || res.Deleted != 0 {
		t.Fatalf("second erase = %+v, %v", res, err)
	}
}

// TestSweeper_ClubClaim (club_claim, 1 year after the decision; BE-057):
// unapproved, the sweep counts and deletes nothing; approved, the decided
// claim goes with its evidence file, a pending claim stays.
func TestSweeper_ClubClaim(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	s := w.student(t, w.uni, false)
	fid := uuid.NewV7()
	w.files.put(files.FileMeta{ID: fid, Purpose: files.PurposeClubClaimEvidence, UploadedBy: &s.UserID, Status: files.StatusActive,
		ScanStatus: files.ScanClean})
	decided, err := w.svc.CreateClubClaim(t.Context(), s, w.club.Club.OrgID, directory.ClaimInput{RoleInClub: "Başkan", EvidenceFileID: &fid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.AdminDecideClubClaim(t.Context(), pad(t), decided.Claim.ID,
		directory.ClaimDecision{Decision: directory.ClaimRejected, Note: "Kanıt yetersiz"}); err != nil {
		t.Fatal(err)
	}
	pending := w.claim(t, w.student(t, w.uni, false), w.sens.Club.OrgID)
	if got := w.mod.Sweeper.PolicyCodes(); !slices.Equal(got, []string{directory.PolicyClubClaim}) {
		t.Fatalf("PolicyCodes() = %v", got)
	}
	later := w.clk.Now().AddDate(1, 0, 1)
	p := privacy.RetentionPolicy{Code: directory.PolicyClubClaim, Retention: later.Sub(later.AddDate(-1, 0, 0))}
	res, err := w.mod.Sweeper.Sweep(t.Context(), p, later)
	if err != nil || res.Count != 1 || len(w.removed.ids) != 0 {
		t.Fatalf("dry run = %+v, %v (removed %v)", res, err, w.removed.ids)
	}
	if _, err := w.mem.GetClaim(t.Context(), decided.Claim.ID); err != nil {
		t.Fatalf("a dry run deleted the claim: %v", err)
	}
	p.LegalApproved = true
	if res, err = w.mod.Sweeper.Sweep(t.Context(), p, later); err != nil || res.Count != 1 || res.Table != "club_claims" {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	if _, err := w.mem.GetClaim(t.Context(), decided.Claim.ID); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("the decided claim survived: %v", err)
	}
	if !slices.Equal(w.removed.ids, []uuid.UUID{fid}) {
		t.Fatalf("removed files = %v, want the evidence", w.removed.ids)
	}
	if _, err := w.mem.GetClaim(t.Context(), pending.ID); err != nil {
		t.Fatalf("the pending claim was purged: %v", err)
	}
	// A year from the decision is not over yet one day earlier.
	if res, _ := w.mod.Sweeper.Sweep(t.Context(), p, later.AddDate(0, 0, -2)); res.Count != 0 {
		t.Fatalf("early sweep = %+v", res)
	}
}
