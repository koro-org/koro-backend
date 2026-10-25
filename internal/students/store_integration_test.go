//go:build integration

package students_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
	"koro/internal/platform/testutil"
	"koro/internal/students"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var seq atomic.Int64

func next() string { return strconv.FormatInt(seq.Add(1), 10) }

func pgExec(t *testing.T, d *db.DB, sql string, args ...any) {
	t.Helper()
	if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// pgWorld inserts the parent rows as app_rw.
func pgWorld(d *db.DB) world {
	return world{
		user: func(t *testing.T) uuid.UUID {
			id := uuid.NewV7()
			pgExec(t, d, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, birth_date, signup_persona)
				VALUES ($1, 'active', $2, $2, 'Ece', 'Şahin', DATE '2004-05-17', 'student')`, id, "s"+next()+"@example.test")
			return id
		},
		university: func(t *testing.T) uuid.UUID {
			org, id, n := uuid.NewV7(), uuid.NewV7(), next()
			pgExec(t, d, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'university', $2, $3)`, org, "Üni "+n, "uni-"+n)
			pgExec(t, d, `INSERT INTO universities (id, org_id, yok_code, name, city, type) VALUES ($1, $2, $3, $4, 'Ankara', 'state')`,
				id, org, "YOK"+n, "Üniversite "+n)
			return id
		},
		department: func(t *testing.T, uni uuid.UUID) uuid.UUID {
			id := uuid.NewV7()
			pgExec(t, d, `INSERT INTO departments (id, university_id, name, degree_level) VALUES ($1, $2, $3, 'bachelor')`, id, uni, "Bölüm "+next())
			return id
		},
		skill: func(t *testing.T) uuid.UUID {
			id, n := uuid.NewV7(), next()
			pgExec(t, d, `INSERT INTO skills (id, slug, name_tr, category) VALUES ($1, $2, $3, 'technical')`, id, "skill-"+n, "Yetkinlik "+n)
			return id
		},
		file: func(t *testing.T) uuid.UUID {
			id := uuid.NewV7()
			pgExec(t, d, `INSERT INTO files (id, purpose, bucket, storage_key, declared_mime) VALUES ($1, 'avatar', 'koro-files-dev', $2, 'image/png')`,
				id, "avatar/2026/10/"+id.String())
			return id
		},
		cvFile: func(t *testing.T) uuid.UUID {
			id := uuid.NewV7()
			pgExec(t, d, `INSERT INTO files (id, purpose, bucket, storage_key, declared_mime) VALUES ($1, 'cv', 'koro-files-dev', $2, 'application/pdf')`,
				id, "cv/2026/10/"+id.String())
			return id
		},
		company: func(t *testing.T) uuid.UUID {
			id, n := uuid.NewV7(), next()
			pgExec(t, d, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'company', $2, $3)`, id, "Şirket "+n, "company-"+n)
			return id
		},
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (students.Store, world) {
		d := testutil.NewDB(t)
		return students.NewGormStore(d.ORM), pgWorld(d)
	})
}

// TestStore_Gorm_FKAndUniqueTranslate: a department that is not in the
// catalog is 422 students.unknown_department (FK), a missing user 404, a
// duplicate skill in one replacement 409/422 (primary key), never a raw
// database error.
func TestStore_Gorm_FKAndUniqueTranslate(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	s, w := students.NewGormStore(d.ORM), pgWorld(d)
	ctx := t.Context()
	uni := w.university(t)
	p, ok, err := s.InsertProfile(ctx, students.Profile{UserID: w.user(t), UniversityID: uni, DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive})
	if err != nil || !ok {
		t.Fatal(err)
	}
	ghost := uuid.NewV7()
	p.DepartmentID = &ghost
	_, err = s.UpdateProfile(ctx, p)
	wantCode(t, err, "students.unknown_department")
	if _, _, err := s.InsertProfile(ctx, students.Profile{UserID: uuid.NewV7(), UniversityID: uni, DegreeLevel: students.DegreeBachelor,
		Status: students.ProfileActive}); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
}

// pgEnv is the students module and the real consent module over one
// PostgreSQL database and one transaction manager (as cmd/koro wires them).
type pgEnv struct {
	*env
	d       *db.DB
	consent consent.Service
	notice  consent.Notice
}

func newPgEnv(t *testing.T) *pgEnv {
	t.Helper()
	d := testutil.NewDB(t)
	clk := clock.NewFake(t0)
	e := newEnv(t) // fakes for directory, orgs, identity, files
	cstore := consent.NewGormStore(d.ORM)
	cmod, err := consent.New(consent.Deps{
		Store: cstore, Tx: d.Tx, Outbox: outbox.NewMem(clk), Audit: &audittest.Recorder{}, Clock: clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()}),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := "# Keşif\n\nProfilimin takma adla keşifte görünmesine izin veriyorum.\n"
	purpose := consent.PurposeDiscoveryProfile
	n := consent.Notice{ID: uuid.NewV7(), Kind: consent.KindConsentText, PurposeCode: &purpose, Audience: consent.Audience("student"),
		Locale: consent.LocaleTR, Version: "2026.10.04", Title: "Keşif rızası", BodyMD: body, BodySHA256: consent.BodyDigest(body),
		CreatedAt: t0.Add(-time.Hour)}
	if err := cstore.InsertNotice(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	if err := cstore.Publish(t.Context(), n.ID, t0.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	mod, err := students.New(students.Deps{
		Store: students.NewGormStore(d.ORM), Tx: d.Tx, Outbox: outbox.NewMem(clk), Audit: e.audit, Clock: clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()}),
		Consent:     cmod.Service, Consents: cmod.Service, Catalog: e.catalog, Orgs: e.orgs, Users: e.users, Verifications: e.users,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	return &pgEnv{env: e, d: d, consent: cmod.Service, notice: n}
}

// pgStudent inserts a student account and returns its actor.
func (e *pgEnv) pgStudent(t *testing.T) authz.Actor {
	t.Helper()
	id := pgWorld(e.d).user(t)
	uni := pgWorld(e.d).university(t)
	e.users.mu.Lock()
	e.users.sum[id] = identitySummary(id)
	e.users.mu.Unlock()
	e.users.verify(id, uni, t0.AddDate(0, -1, 0))
	return authz.Actor{Kind: authz.KindUser, UserID: id, StudentStatus: authz.StudentVerified, UniversityID: &uni}
}

// pgStudentAt inserts a student account verified at uni and returns its
// actor.
func (e *pgEnv) pgStudentAt(t *testing.T, uni uuid.UUID, minor bool) authz.Actor {
	t.Helper()
	id := pgWorld(e.d).user(t)
	s := identitySummary(id)
	s.IsMinor = minor
	e.users.mu.Lock()
	e.users.sum[id] = s
	e.users.mu.Unlock()
	e.users.verify(id, uni, t0.AddDate(0, -1, 0))
	return authz.Actor{Kind: authz.KindUser, UserID: id, StudentStatus: authz.StudentVerified, UniversityID: &uni, IsMinor: minor}
}

// pgCohort creates n students verified at uni with class year year (the
// service writes their profiles and keys) and members of the sensitive club.
func (e *pgEnv) pgCohort(t *testing.T, uni uuid.UUID, n int, year int16, minor bool) []authz.Actor {
	t.Helper()
	out := make([]authz.Actor, n)
	for i := range out {
		a := e.pgStudentAt(t, uni, minor)
		y := year
		if _, err := e.svc.UpdateMyProfile(t.Context(), a, students.ProfilePatch{ClassYear: &y}); err != nil {
			t.Fatal(err)
		}
		e.orgs.join(a.UserID, e.clubSens, authz.MemberMember)
		out[i] = a
	}
	return out
}

// TestUpdatePrivacy_ConsentFailureRollsBack_Postgres (02 §9.2, T-33): the
// setting and the consent row commit together or not at all. A wrong
// consent text fails the consent record, and the settings row (created by
// the same request) is rolled back with it; the right text writes both,
// and the withdrawal writes the setting and the withdrawal row together.
func TestUpdatePrivacy_ConsentFailureRollsBack_Postgres(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	ctx := t.Context()
	a := e.pgStudent(t)
	count := func() int {
		var n int
		if err := e.d.Pool.QueryRow(ctx, `SELECT count(*) FROM consents WHERE user_id = $1 AND purpose_code = 'discovery_profile'`,
			a.UserID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	wrong := make([]byte, 32)
	_, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{Discoverable: ptr(true),
		DiscoveryConsent: &students.ConsentProof{NoticeVersionID: e.notice.ID, TextSHA256: wrong}, Channel: consent.ChannelWeb})
	wantCode(t, err, "consent.text_mismatch")
	if _, err := students.NewGormStore(e.d.ORM).GetPrivacy(ctx, a.UserID); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("settings row survived the failed consent: %v", err)
	}
	if count() != 0 {
		t.Fatal("consent row without the setting")
	}
	got, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{Discoverable: ptr(true),
		DiscoveryConsent: &students.ConsentProof{NoticeVersionID: e.notice.ID, TextSHA256: e.notice.BodySHA256}, Channel: consent.ChannelWeb})
	if err != nil || !got.Discoverable || count() != 1 {
		t.Fatalf("grant = %+v, %v (consents %d)", got, err, count())
	}
	if ok, err := e.consent.HasActive(ctx, a.UserID, consent.PurposeDiscoveryProfile, nil); err != nil || !ok {
		t.Fatalf("consent not in force: %v %v", ok, err)
	}
	if got, err = e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{Discoverable: ptr(false)}); err != nil || got.Discoverable || count() != 2 {
		t.Fatalf("withdraw = %+v, %v (consents %d)", got, err, count())
	}
	if ok, _ := e.consent.HasActive(ctx, a.UserID, consent.PurposeDiscoveryProfile, nil); ok {
		t.Fatal("consent still in force after turning discoverable off")
	}
}

// TestAudience_Gorm_EndToEnd: the profile, its keys and the nightly
// refresh against PostgreSQL; count_segment sees the eligible profile and
// never the sensitive club.
func TestAudience_Gorm_EndToEnd(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	ctx := t.Context()
	a := e.pgStudent(t)
	dept := pgWorld(e.d).department(t, *a.UniversityID)
	e.catalog.mu.Lock()
	e.catalog.depts[dept] = deptOf(dept, *a.UniversityID)
	e.catalog.mu.Unlock()
	e.orgs.join(a.UserID, e.clubPlain, authz.MemberMember)
	e.orgs.join(a.UserID, e.clubSens, authz.MemberMember)
	year := int16(2)
	if _, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Department: &dept, ClassYear: &year}); err != nil {
		t.Fatal(err)
	}
	p, err := students.NewGormStore(e.d.ORM).GetProfile(ctx, a.UserID)
	if err != nil || !p.AudienceEligible || slices.Contains(p.AudienceKeys, students.ClubKey(e.clubSens)) ||
		!slices.Contains(p.AudienceKeys, students.ClubKey(e.clubPlain)) {
		t.Fatalf("stored = %+v, %v", p, err)
	}
	var n int
	if err := e.d.Pool.QueryRow(ctx, `SELECT count(*) FROM student_profiles WHERE audience_eligible AND audience_keys && $1::text[]`,
		db.TextArray{students.ClubKey(e.clubSens)}).Scan(&n); err != nil || n != 0 {
		t.Fatalf("profiles targetable by the sensitive club = %d, %v", n, err)
	}
	e.users.lapse(a.UserID)
	for _, j := range e.mod.Jobs() {
		if j.Name == students.AudienceRefreshJobName {
			if err := j.Handle(context.WithoutCancel(ctx)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if p, _ := students.NewGormStore(e.d.ORM).GetProfile(ctx, a.UserID); p.AudienceEligible {
		t.Fatal("eligible after the verification lapsed")
	}
}

func identitySummary(id uuid.UUID) identity.UserSummary {
	return identity.UserSummary{ID: id, Status: identity.StatusActive, FirstName: "Ece", LastName: "Şahin"}
}

func deptOf(id, uni uuid.UUID) directory.Department {
	return directory.Department{ID: id, UniversityID: uni, Name: "Bölüm", DegreeLevel: directory.DegreeBachelor, IsActive: true}
}

// TestPrivacyProvider_Gorm_CoversEveryTable (01 §3.3; personal_tables.txt
// provider:students): against PostgreSQL the provider exports and erases
// all five tables of BE-023 (student_profiles, student_privacy_settings,
// student_skills, student_experiences, student_languages); a second
// erasure finds nothing and another user's rows stay.
func TestPrivacyProvider_Gorm_CoversEveryTable(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	ctx := t.Context()
	w := pgWorld(e.d)
	skill := w.skill(t)
	e.catalog.mu.Lock()
	e.catalog.skills[skill] = true
	e.catalog.mu.Unlock()
	a, other := e.pgStudent(t), e.pgStudent(t)
	for _, x := range []authz.Actor{a, other} {
		if _, err := e.svc.UpdateMyProfile(ctx, x, students.ProfilePatch{Headline: ptr("Başlık")}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.PutMySkills(ctx, x, []students.SkillInput{{SkillID: skill}}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.PutMyLanguages(ctx, x, []students.LanguageInput{{LanguageCode: "en", Level: "B2"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.CreateMyExperience(ctx, x, students.ExperienceInput{Kind: students.KindJob, Title: "İş", StartMonth: ym(2025, 1)}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.UpdateMyPrivacy(ctx, x, students.PrivacyPatch{ShowClubsOnGrant: ptr(true)}); err != nil {
			t.Fatal(err)
		}
	}
	tables := []string{"student_profiles", "student_privacy_settings", "student_skills", "student_experiences", "student_languages"}
	rows := func(user uuid.UUID) map[string]int {
		out := map[string]int{}
		for _, tb := range tables {
			var n int
			if err := e.d.Pool.QueryRow(ctx, `SELECT count(*) FROM `+tb+` WHERE user_id = $1`, user).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[tb] = n
		}
		return out
	}
	for tb, n := range rows(a.UserID) {
		if n != 1 {
			t.Fatalf("setup: %s has %d rows", tb, n)
		}
	}
	sec, err := e.mod.Privacy.Export(ctx, a.UserID)
	if err != nil {
		t.Fatal(err)
	}
	var records []string
	for _, it := range sec.Items {
		records = append(records, it["record"].(string))
	}
	slices.Sort(records)
	if want := []string{"experience", "language", "privacy_settings", "skill", "student_profile"}; !slices.Equal(records, want) {
		t.Fatalf("export records = %v", records)
	}
	res, err := e.mod.Privacy.Erase(ctx, a.UserID)
	if err != nil || res.Deleted != 5 {
		t.Fatalf("Erase = %+v, %v", res, err)
	}
	for tb, n := range rows(a.UserID) {
		if n != 0 {
			t.Fatalf("%s kept %d rows after the erasure", tb, n)
		}
	}
	if res, err := e.mod.Privacy.Erase(ctx, a.UserID); err != nil || res.Deleted != 0 {
		t.Fatalf("second Erase = %+v, %v", res, err)
	}
	for tb, n := range rows(other.UserID) {
		if n != 1 {
			t.Fatalf("another user's %s rows = %d", tb, n)
		}
	}
}

// pgFiles is the real files module over the database of e (in-memory
// object storage): CVs are attached, served and deleted through it, with
// the foreign keys of student_cvs and student_profiles in force.
func (e *pgEnv) pgFiles(t *testing.T) (files.Service, files.Store, *storage.Mem) {
	t.Helper()
	clk := clock.NewFake(t0)
	mem := storage.NewMem(clk)
	fstore := files.NewGormStore(e.d.ORM)
	mod, err := files.New(files.Deps{
		Store: fstore, Objects: mem, Tx: e.d.Tx, Outbox: outbox.NewMem(clk), Queue: queue.NewMem(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: e.d.Tx, Store: idempotency.NewGormStore(e.d.ORM), Clock: clk, Log: obs.Discard()}),
		Clock:       clk, Log: obs.Discard(), Buckets: files.Buckets{Files: "pg-files", Quarantine: "pg-quarantine"}, Audit: &audittest.Recorder{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return mod.Service, fstore, mem
}

// pgCVUpload stores an active, scanned cv upload of owner in the files
// table and the object storage.
func pgCVUpload(t *testing.T, st files.Store, mem *storage.Mem, owner uuid.UUID) uuid.UUID {
	t.Helper()
	id, size, body := uuid.NewV7(), int64(len(pgPDF)), pgPDF
	key := "cv/2026/10/" + id.String()
	name := "ozgecmis.pdf"
	f := files.File{ID: id, Purpose: files.PurposeCV, OwnerUserID: &owner, UploadedBy: &owner, Bucket: "pg-files", StorageKey: key,
		OriginalName: &name, DeclaredMIME: files.MIMEPDF, SizeBytes: &size, SHA256: make([]byte, 32), Status: files.StatusActive,
		ScanStatus: files.ScanClean, CreatedAt: t0, UpdatedAt: t0}
	if err := st.Insert(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	mem.Upload("pg-files", key, body, files.MIMEPDF)
	return id
}

var pgPDF = []byte("%PDF-1.7\n% students integration\n%%EOF\n")

// TestCV_Gorm_EndToEnd: a CV over PostgreSQL with the real files module:
// attached to its row, served by a ≤ 5-minute URL, the fourth refused
// (409), deleted with its files row in one transaction (student_cvs →
// files foreign key order), and a rejected CV dropped by file.rejected.
func TestCV_Gorm_EndToEnd(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	ctx := t.Context()
	fsvc, fstore, mem := e.pgFiles(t)
	mod, err := students.New(students.Deps{
		Store: students.NewGormStore(e.d.ORM), Tx: e.d.Tx, Outbox: outbox.NewMem(e.clk), Audit: e.audit, Clock: e.clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: e.d.Tx, Store: idempotency.NewGormStore(e.d.ORM), Clock: e.clk, Log: obs.Discard()}),
		Consent:     e.consent, Consents: e.consent, Catalog: e.catalog, Orgs: e.orgs, Users: e.users, Verifications: e.users,
		Files: fsvc,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := mod.Service
	a := e.pgStudent(t)
	var cvs []students.CVView
	for range 3 {
		v, err := svc.CreateMyCV(ctx, a, pgCVUpload(t, fstore, mem, a.UserID), false)
		if err != nil {
			t.Fatal(err)
		}
		cvs = append(cvs, v)
	}
	if f, err := fsvc.Get(ctx, cvs[0].CV.FileID); err != nil || f.AttachedModule != "students" || f.AttachedEntityID != cvs[0].CV.ID {
		t.Fatalf("attachment = %+v, %v", f, err)
	}
	if !cvs[0].CV.IsCurrent || cvs[0].FileName != "ozgecmis.pdf" {
		t.Fatalf("first CV = %+v", cvs[0])
	}
	fourth := pgCVUpload(t, fstore, mem, a.UserID)
	_, err = svc.CreateMyCV(ctx, a, fourth, true)
	wantCode(t, err, "students.cv_limit")
	if f, _ := fsvc.Get(ctx, fourth); f.AttachedModule != "" {
		t.Fatal("the refused CV's file was attached (no rollback)")
	}
	dl, err := svc.DownloadMyCV(ctx, a, cvs[1].CV.ID)
	if err != nil || dl.URL == "" || dl.ExpiresAt.After(e.clk.Now().Add(5*time.Minute)) {
		t.Fatalf("download = %+v, %v", dl, err)
	}
	if err := svc.DeleteMyCV(ctx, a, cvs[1].CV.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fsvc.Get(ctx, cvs[1].CV.FileID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("files row of the deleted CV: %v", err)
	}
	// file.rejected for the current CV: the row and the file go.
	subs := outbox.NewRegistry()
	mod.Subscribe(subs)
	entity := cvs[0].CV.ID
	env, err := domainevent.New(domainevent.FileRejected{FileID: cvs[0].CV.FileID, UploaderID: &a.UserID, Purpose: "cv",
		Reason: "infected", AttachedModule: "students", AttachedEntityID: &entity}, "file", cvs[0].CV.FileID)
	if err != nil {
		t.Fatal(err)
	}
	env.ID, env.OccurredAt = uuid.NewV7(), t0
	body, _ := json.Marshal(env)
	for _, s := range subs.For(domainevent.TopicFileRejected) {
		if s.Name == students.DropRejectedFileTaskName {
			if err := s.HandleTask(context.WithoutCancel(ctx), body); err != nil {
				t.Fatal(err)
			}
		}
	}
	left, err := students.NewGormStore(e.d.ORM).CVs(ctx, a.UserID)
	if err != nil || len(left) != 1 || left[0].ID != cvs[2].CV.ID || left[0].IsCurrent {
		t.Fatalf("CVs after the rejection = %+v, %v", left, err)
	}
	if _, err := fsvc.Get(ctx, cvs[0].CV.FileID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("rejected file kept: %v", err)
	}
	// The account erasure takes the last CV and its file.
	if _, err := mod.Privacy.Erase(ctx, a.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := fsvc.Get(ctx, cvs[2].CV.FileID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("erased account's CV file kept: %v", err)
	}
}

// TestAudience_Gorm_CountSegment: Count and Stream over PostgreSQL go
// through count_segment (NULL below 10) and its predicate: minors and the
// sensitive club never count, the dimension count comes from the keys.
func TestAudience_Gorm_CountSegment(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	ctx := t.Context()
	uni := pgWorld(e.d).university(t)
	e.catalog.mu.Lock()
	e.catalog.unis[uni] = directory.University{ID: uni, Name: "PG Üniversitesi", IsActive: true}
	e.catalog.mu.Unlock()
	year2 := e.pgCohort(t, uni, 9, 2, false)
	e.pgCohort(t, uni, 4, 2, true)
	uniKey := "uni:" + uni.String()
	if _, err := e.svc.Count(ctx, students.AudienceSpec{Keys: []string{uniKey}}); !errors.Is(err, students.ErrAudienceTooSmall) {
		t.Fatalf("9 adults and 4 minors: %v, want too small", err)
	}
	year3 := e.pgCohort(t, uni, 2, 3, false)
	if n, err := e.svc.Count(ctx, students.AudienceSpec{Keys: []string{uniKey}}); err != nil || n != 11 {
		t.Fatalf("Count = %d, %v; want 11 adults", n, err)
	}
	if _, err := e.svc.Count(ctx, students.AudienceSpec{Keys: []string{uniKey, "year:3"}}); !errors.Is(err, students.ErrAudienceTooSmall) {
		t.Fatalf("uni ∧ year 3 (2): %v", err)
	}
	if _, err := e.svc.Count(ctx, students.AudienceSpec{Keys: []string{students.ClubKey(e.clubSens)}}); !errors.Is(err, students.ErrAudienceTooSmall) {
		t.Fatalf("sensitive club: %v", err)
	}
	var got []uuid.UUID
	if err := e.svc.Stream(ctx, students.AudienceSpec{Keys: []string{uniKey, "year:2", "year:3"}}, func(id uuid.UUID) error {
		got = append(got, id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := ids(append(year2, year3...)); !slices.Equal(got, want) {
		t.Fatalf("streamed %v, want %v", got, want)
	}
}

// TestGrants_Postgres_ConcurrentAndScoped (02 §9.7; BE-046): concurrent
// acceptances of several contact requests of one company leave exactly one
// active grant (the student's advisory lock); a grant to an unknown org or
// to a club is not found (the composite FK binds it to a company); the
// student's revocation ends it in the database at once.
func TestGrants_Postgres_ConcurrentAndScoped(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	ctx := t.Context()
	w := pgWorld(e.d)
	a := e.pgStudent(t)
	company := w.company(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- e.svc.Grant(ctx, a.UserID, company, students.SourceContactRequest, uuid.NewV7()) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Grant: %v", err)
		}
	}
	var active int
	if err := e.d.Pool.QueryRow(ctx, `SELECT count(*) FROM profile_access_grants WHERE student_user_id = $1 AND revoked_at IS NULL`,
		a.UserID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active grants = %d, %v; want 1", active, err)
	}
	club := uuid.NewV7()
	pgExec(t, e.d, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'club', 'Kulüp', $2)`, club, "club-"+next())
	for name, org := range map[string]uuid.UUID{"unknown org": uuid.NewV7(), "club": club} {
		if err := e.svc.Grant(ctx, a.UserID, org, students.SourceContactRequest, uuid.NewV7()); !errors.Is(err, students.ErrNotFound) {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}
	mine, err := e.svc.ListMyAccessGrants(ctx, a, nil, 0)
	if err != nil || len(mine.Items) != 1 {
		t.Fatalf("my grants = %+v, %v", mine, err)
	}
	if err := e.svc.RevokeMyAccessGrant(ctx, a, mine.Items[0].Grant.ID, "Gerek kalmadı"); err != nil {
		t.Fatal(err)
	}
	var kind, reason string
	if err := e.d.Pool.QueryRow(ctx, `SELECT revoked_by_kind, revoke_reason FROM profile_access_grants WHERE id = $1`,
		mine.Items[0].Grant.ID).Scan(&kind, &reason); err != nil || kind != "student" || reason != "Gerek kalmadı" {
		t.Fatalf("stored revocation = %q %q, %v", kind, reason, err)
	}
	if ok, err := e.svc.Has(ctx, a.UserID, company); err != nil || ok {
		t.Fatalf("Has = %v, %v", ok, err)
	}
}
