package identity_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/civil"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/token"
)

// Age gate of the student verification and the one age rule (Q-52,
// BE-202): no account becomes a student without a birth date, and a
// student whose age is unknown gets the protections of users under 18.

// noBirthUser is an active account without a birth date (a company
// persona) and its actor, no student status.
func (e *eduEnv) noBirthUser(t *testing.T, email string) (identity.User, authz.Actor) {
	t.Helper()
	now := e.clk.Now().UTC().Truncate(time.Microsecond)
	u := identity.User{
		ID: uuid.NewV7(), Status: identity.StatusActive, Email: email, EmailNormalized: email, EmailVerifiedAt: &now,
		PasswordHash: goodPasswordHash(), FirstName: "Ada", LastName: "Yılmaz", Persona: identity.PersonaCompany,
		Locale: identity.LocaleTR,
	}
	if err := e.store.InsertUser(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	return u, authz.Actor{Kind: authz.KindUser, UserID: u.ID, SessionID: uuid.NewV7(), StudentStatus: authz.StudentNone}
}

// verifiedRow writes a verification of userID in status st (verified or
// expired), as rows written before the age gate covered every path.
func (e *eduEnv) verifiedRow(t *testing.T, userID uuid.UUID, st identity.EduStatus) {
	t.Helper()
	at := e.clk.Now().UTC().Truncate(time.Microsecond).AddDate(0, -1, 0)
	exp, uni, addr := at.AddDate(1, 0, 0), eduUni, uuid.NewV7().String()+"@ogr.uni.edu.tr"
	if st == identity.EduExpired {
		exp = at.Add(time.Hour)
	}
	v := identity.EduRecord{ID: uuid.NewV7(), UserID: userID, Method: identity.EduMethodEmail, EduEmail: addr,
		EduEmailNormalized: addr, EmailDomain: "ogr.uni.edu.tr", UniversityID: &uni, Status: st, VerifiedAt: &at, ExpiresAt: &exp}
	if err := e.store.InsertEduVerification(t.Context(), v); err != nil {
		t.Fatal(err)
	}
}

func birthDate(t *testing.T, y int, m time.Month, d int) *civil.Date {
	t.Helper()
	b, err := civil.New(y, m, d)
	if err != nil {
		t.Fatal(err)
	}
	return &b
}

func wantField(t *testing.T, err error, want *problem.Error, location string) {
	t.Helper()
	problemtest.WantErr(t, err, want)
	pe, _ := errors.AsType[*problem.Error](err)
	if fs := pe.Fields(); len(fs) != 1 || fs[0].Location != location {
		t.Fatalf("fields = %+v, want one at %s", fs, location)
	}
}

func TestMinorProtected_UnknownAgeOfStudentIsMinor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	adult, _ := civil.New(2000, time.March, 14)
	minor, _ := civil.New(2010, time.March, 14)
	eighteenToday, _ := civil.New(2008, time.October, 3)
	for _, c := range []struct {
		name    string
		birth   *civil.Date
		student bool
		want    bool
	}{
		{"student, age unknown", nil, true, true},
		{"never a student, age unknown", nil, false, false},
		{"minor student", &minor, true, true},
		{"minor, never a student", &minor, false, true},
		{"adult student", &adult, true, false},
		{"adult, never a student", &adult, false, false},
		{"18 today", &eighteenToday, true, false},
	} {
		if got := identity.MinorProtected(c.birth, c.student, now); got != c.want {
			t.Errorf("%s: MinorProtected = %v, want %v", c.name, got, c.want)
		}
	}
	for st, want := range map[authz.StudentStatus]bool{
		authz.StudentVerified: true, authz.StudentLapsed: true, authz.StudentNone: false, "": false,
	} {
		if got := identity.WasStudent(st); got != want {
			t.Errorf("WasStudent(%q) = %v, want %v", st, got, want)
		}
	}
}

func TestStartEdu_BirthDateRequiredWithoutOne(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	ctx := t.Context()
	u, a := e.noBirthUser(t, "sirket@example.com")
	err := e.svc.StartEduVerification(ctx, a, identity.EduStart{Email: "ada@ogr.uni.edu.tr"})
	wantField(t, err, identity.ErrBirthDateRequired, "body.birth_date")
	future := civil.Today(e.clk.Now()).AddDays(1)
	err = e.svc.StartEduVerification(ctx, a, identity.EduStart{Email: "ada@ogr.uni.edu.tr", BirthDate: &future})
	wantField(t, err, identity.ErrInvalidBirthDate, "body.birth_date")
	e.deliver(t)
	if n := e.mailsOf("identity.edu_verify"); n != 0 {
		t.Fatalf("%d verification e-mails without a birth date", n)
	}
	if got, _ := e.store.User(ctx, u.ID); got.BirthDate != nil {
		t.Fatal("a refused start wrote the birth date")
	}

	given := birthDate(t, 2005, time.May, 20)
	if err := e.svc.StartEduVerification(ctx, a, identity.EduStart{Email: "ada@ogr.uni.edu.tr", BirthDate: given}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.store.User(ctx, u.ID); got.BirthDate == nil || *got.BirthDate != *given {
		t.Fatalf("birth date = %v, want %v", got.BirthDate, given)
	}
	meta := e.lastAudit(t, authz.IdentityEduVerificationStart)
	if meta["birth_date_set"] != true {
		t.Fatalf("start audit = %v, want birth_date_set", meta)
	}
	for _, v := range meta {
		if s, ok := v.(string); ok && strings.Contains(s, given.String()) {
			t.Fatal("the audit record holds the birth date")
		}
	}
	if err := e.svc.ConfirmEduVerification(ctx, authz.Anonymous(), e.lastToken(t, "identity.edu_verify")); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if rows := e.eduRows(t, u.ID); len(rows) != 1 || rows[0].Status != identity.EduVerified {
		t.Fatalf("rows = %+v", rows)
	}

	// A birth date is set once: a renewal's birth_date is ignored.
	other := birthDate(t, 1999, time.January, 1)
	if err := e.svc.StartEduVerification(ctx, a, identity.EduStart{Email: "ada@uni.edu.tr", BirthDate: other}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.store.User(ctx, u.ID); *got.BirthDate != *given {
		t.Fatalf("renewal replaced the birth date: %v", got.BirthDate)
	}
	if _, set := e.lastAudit(t, authz.IdentityEduVerificationStart)["birth_date_set"]; set {
		t.Fatal("renewal audit says birth_date_set")
	}
}

func TestSubmitEduDocument_BirthDateRequiredWithoutOne(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	ctx := t.Context()
	u, a := e.noBirthUser(t, "sirket@example.com")
	doc := e.files.add(u.ID, files.PurposeEduDocument, files.ScanClean)
	_, err := e.svc.SubmitEduDocument(ctx, a, identity.EduDocumentSubmission{UniversityID: eduUni, FileID: doc})
	wantField(t, err, identity.ErrBirthDateRequired, "body.birth_date")
	if rows := e.eduRows(t, u.ID); len(rows) != 0 {
		t.Fatalf("rows without a birth date: %+v", rows)
	}
	given := birthDate(t, 2004, time.February, 29)
	v, err := e.svc.SubmitEduDocument(ctx, a, identity.EduDocumentSubmission{UniversityID: eduUni, FileID: doc, BirthDate: given})
	if err != nil {
		t.Fatal(err)
	}
	if v.Record.Status != identity.EduPending {
		t.Fatalf("status %s", v.Record.Status)
	}
	if got, _ := e.store.User(ctx, u.ID); got.BirthDate == nil || *got.BirthDate != *given {
		t.Fatalf("birth date = %v", got.BirthDate)
	}
	if e.lastAudit(t, authz.IdentityEduVerificationSubmitDocument)["birth_date_set"] != true {
		t.Fatal("submit audit without birth_date_set")
	}
}

func TestConfirmEdu_AccountWithoutBirthDate_Conflict(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	ctx := t.Context()
	u, a := e.noBirthUser(t, "sirket@example.com")
	// A link sent before the age gate covered every path.
	raw := strings.Repeat("B", 52)
	now := e.clk.Now().UTC().Truncate(time.Microsecond)
	tok := identity.EmailToken{ID: uuid.NewV7(), UserID: u.ID, Purpose: identity.PurposeEduEmail, TargetEmail: "ada@ogr.uni.edu.tr",
		KeyID: "k1", TokenHash: token.Hash(raw), ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now}
	if err := e.store.InsertEmailToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	problemtest.WantErr(t, e.svc.ConfirmEduVerification(ctx, authz.Anonymous(), raw), identity.ErrBirthDateMissing)
	if rows := e.eduRows(t, u.ID); len(rows) != 0 {
		t.Fatalf("verified without a birth date: %+v", rows)
	}
	if got, found, _ := e.store.EmailTokenByHash(ctx, token.Hash(raw)); !found || got.ConsumedAt != nil {
		t.Fatalf("token after the conflict = %+v %v, want unused", got, found)
	}
	// The holder adds the birth date and opens the same link again.
	if _, err := e.svc.UpdateMe(ctx, a, identity.MePatch{BirthDate: birthDate(t, 2003, time.April, 23)}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ConfirmEduVerification(ctx, authz.Anonymous(), raw); err != nil {
		t.Fatalf("confirm after the birth date: %v", err)
	}
	if rows := e.eduRows(t, u.ID); len(rows) != 1 || rows[0].Status != identity.EduVerified {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestAdminDecide_ApplicantWithoutBirthDate_Conflict(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	ctx := t.Context()
	u, a := e.noBirthUser(t, "sirket@example.com")
	// A document submitted before the age gate covered every path.
	doc, uni := e.files.add(u.ID, files.PurposeEduDocument, files.ScanClean), eduUni
	pending := identity.EduRecord{ID: uuid.NewV7(), UserID: u.ID, Method: identity.EduMethodDocument, UniversityID: &uni,
		DocumentFileID: &doc, Status: identity.EduPending}
	if err := e.store.InsertEduVerification(ctx, pending); err != nil {
		t.Fatal(err)
	}
	pad := e.staff(t, "admin@example.com", authz.PlatformAdmin)
	approve := identity.EduDecision{Decision: identity.EduVerified, UniversityID: &uni}
	_, err := e.svc.DecideEduVerification(ctx, pad, pending.ID, approve)
	problemtest.WantErr(t, err, identity.ErrBirthDateMissing)
	if got, _ := e.store.EduVerification(ctx, pending.ID); got.Status != identity.EduPending {
		t.Fatalf("status after the conflict = %s, want pending", got.Status)
	}
	if _, err := e.svc.UpdateMe(ctx, a, identity.MePatch{BirthDate: birthDate(t, 2003, time.April, 23)}); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.DecideEduVerification(ctx, pad, pending.ID, approve)
	if err != nil || got.Record.Status != identity.EduVerified {
		t.Fatalf("approve after the birth date = %+v, %v", got.Record, err)
	}
}

func TestUpdateMe_BirthDateSetOnce(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	ctx := t.Context()
	u, a := e.noBirthUser(t, "sirket@example.com")
	future := civil.Today(e.clk.Now()).AddDays(1)
	_, err := e.svc.UpdateMe(ctx, a, identity.MePatch{BirthDate: &future})
	wantField(t, err, identity.ErrInvalidBirthDate, "body.birth_date")

	given := birthDate(t, 2003, time.April, 23)
	me, err := e.svc.UpdateMe(ctx, a, identity.MePatch{BirthDate: given})
	if err != nil || me.BirthDate == nil || *me.BirthDate != *given {
		t.Fatalf("UpdateMe = %+v, %v", me.BirthDate, err)
	}
	audits := e.audit.of(string(authz.IdentityAccountUpdateOwn))
	if len(audits) != 1 || !slices.Equal(audits[0].Metadata["fields"].([]string), []string{"birth_date"}) {
		t.Fatalf("audit = %+v", audits)
	}
	// The same date again changes nothing.
	if _, err := e.svc.UpdateMe(ctx, a, identity.MePatch{BirthDate: given}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.audit.of(string(authz.IdentityAccountUpdateOwn))); n != 1 {
		t.Fatalf("%d audits after a no-op", n)
	}
	// Another date is refused, with whatever else the patch carries.
	name := "Ayşe"
	_, err = e.svc.UpdateMe(ctx, a, identity.MePatch{FirstName: &name, BirthDate: birthDate(t, 1990, time.January, 1)})
	problemtest.WantErr(t, err, identity.ErrBirthDateLocked)
	if got, _ := e.store.User(ctx, u.ID); *got.BirthDate != *given || got.FirstName != "Ada" {
		t.Fatalf("after a refused patch: birth %v, name %q", got.BirthDate, got.FirstName)
	}
	// A student's registration birth date is locked the same way.
	s, sa := e.student(t, "ada@example.com")
	_, err = e.svc.UpdateMe(ctx, sa, identity.MePatch{BirthDate: birthDate(t, 1990, time.January, 1)})
	problemtest.WantErr(t, err, identity.ErrBirthDateLocked)
	if got, _ := e.store.User(ctx, s.ID); *got.BirthDate != *s.BirthDate {
		t.Fatal("a student's birth date changed")
	}
}

func TestMe_StudentWithoutBirthDate_ProtectedAndPrompted(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	ctx := t.Context()
	u, a := e.noBirthUser(t, "eski@example.com")
	e.verifiedRow(t, u.ID, identity.EduVerified)
	uni := eduUni
	a.StudentStatus, a.UniversityID = authz.StudentVerified, &uni
	me, err := e.svc.Me(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if !me.IsMinor || !me.BirthDateRequired || slices.Contains(me.Capabilities, authz.DirectoryClubClaimCreate) {
		t.Fatalf("Me = minor %v, required %v, capabilities %v", me.IsMinor, me.BirthDateRequired, me.Capabilities)
	}
	me, err = e.svc.UpdateMe(ctx, a, identity.MePatch{BirthDate: birthDate(t, 2003, time.April, 23)})
	if err != nil {
		t.Fatal(err)
	}
	if me.IsMinor || me.BirthDateRequired || !slices.Contains(me.Capabilities, authz.DirectoryClubClaimCreate) {
		t.Fatalf("Me after the birth date = minor %v, required %v, capabilities %v", me.IsMinor, me.BirthDateRequired, me.Capabilities)
	}
	// An account that never was a student is not asked and not protected.
	_, other := e.noBirthUser(t, "sirket@example.com")
	if me, err := e.svc.Me(ctx, other); err != nil || me.IsMinor || me.BirthDateRequired {
		t.Fatalf("non-student Me = minor %v, required %v, %v", me.IsMinor, me.BirthDateRequired, err)
	}
}

func TestUsers_UnknownAgeOfStudentIsMinor(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	ctx := t.Context()
	verified, _ := e.noBirthUser(t, "v@example.com")
	e.verifiedRow(t, verified.ID, identity.EduVerified)
	lapsed, _ := e.noBirthUser(t, "l@example.com")
	e.verifiedRow(t, lapsed.ID, identity.EduExpired)
	pending, _ := e.noBirthUser(t, "p@example.com")
	doc, uni := e.files.add(pending.ID, files.PurposeEduDocument, files.ScanClean), eduUni
	if err := e.store.InsertEduVerification(ctx, identity.EduRecord{ID: uuid.NewV7(), UserID: pending.ID,
		Method: identity.EduMethodDocument, UniversityID: &uni, DocumentFileID: &doc, Status: identity.EduPending}); err != nil {
		t.Fatal(err)
	}
	company, _ := e.noBirthUser(t, "c@example.com")
	adult, _ := e.student(t, "a@example.com")
	minor := e.activeUserBorn(t, "m@example.com", birthDate(t, 2011, time.March, 1))

	want := map[uuid.UUID]bool{verified.ID: true, lapsed.ID: true, pending.ID: false, company.ID: false, adult.ID: false, minor.ID: true}
	ids := make([]uuid.UUID, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	got, err := e.svc.GetMany(ctx, ids)
	if err != nil || len(got) != len(want) {
		t.Fatalf("GetMany = %d, %v", len(got), err)
	}
	for id, w := range want {
		if got[id].IsMinor != w {
			t.Errorf("GetMany %s: IsMinor = %v, want %v", got[id].LastName+" "+id.String(), got[id].IsMinor, w)
		}
		one, err := e.svc.Get(ctx, id)
		if err != nil || one.IsMinor != w {
			t.Errorf("Get %s: IsMinor = %v, %v; want %v", id, one.IsMinor, err, w)
		}
	}
}

// activeUserBorn is activeUser with another birth date.
func (e *eduEnv) activeUserBorn(t *testing.T, email string, birth *civil.Date) identity.User {
	t.Helper()
	r := e.registration(email)
	r.BirthDate = birth
	if err := e.svc.Register(t.Context(), authz.Anonymous(), r); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ConfirmEmail(t.Context(), authz.Anonymous(), e.lastToken(t, "identity.verify_email")); err != nil {
		t.Fatal(err)
	}
	id, _, err := e.svc.FindByEmail(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	u, err := e.store.User(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
