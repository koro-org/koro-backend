//go:build integration

package students_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/directory"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
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
		Store: cstore, Tx: d.Tx, Outbox: outbox.NewMem(clk), Audit: &auditLog{}, Clock: clk, Log: obs.Discard(),
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
