package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
)

// eduUni is the university of the fake directory: ogr.uni.edu.tr and
// uni.edu.tr resolve to it (subdomains like BE-020's resolver).
var eduUni = uuid.MustParse("01900000-0000-7000-8000-00000000e0e0")

func fakeDomains(_ context.Context, domain string) (uuid.UUID, bool, error) {
	if domain == "uni.edu.tr" || strings.HasSuffix(domain, ".uni.edu.tr") {
		return eduUni, true, nil
	}
	return uuid.Nil(), false, nil
}

// fakeFiles is the files port of identity (Get, Attach, PresignGet) plus
// the remover.
type fakeFiles struct {
	mu       sync.Mutex
	clk      *clock.Fake
	metas    map[uuid.UUID]files.FileMeta
	ttls     []time.Duration
	removed  []uuid.UUID
	attachTx []any
}

func (f *fakeFiles) add(uploader uuid.UUID, purpose files.Purpose, scan files.ScanStatus) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, by := uuid.NewV7(), uploader
	f.metas[id] = files.FileMeta{ID: id, Purpose: purpose, OwnerUserID: &by, UploadedBy: &by, Status: files.StatusActive,
		ScanStatus: scan, AttachedEntityID: uuid.Nil()}
	return id
}

func (f *fakeFiles) Get(_ context.Context, id uuid.UUID) (files.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.metas[id]
	if !ok {
		return files.FileMeta{}, files.ErrNotFound
	}
	return m, nil
}

func (f *fakeFiles) Attach(ctx context.Context, _ authz.Actor, id uuid.UUID, o files.Owner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.metas[id]
	switch {
	case !ok:
		return files.ErrNotFound
	case m.Purpose != o.Purpose:
		return files.ErrPurposeMismatch
	case m.AttachedModule != "" && (m.AttachedModule != o.Module || m.AttachedEntityID != o.EntityID):
		return files.ErrAlreadyAttached
	}
	m.AttachedModule, m.AttachedEntityID = o.Module, o.EntityID
	f.metas[id] = m
	f.attachTx = append(f.attachTx, ctx.Value(txKey{}))
	return nil
}

func (f *fakeFiles) PresignGet(_ context.Context, id uuid.UUID, ttl time.Duration) (files.Download, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.metas[id]
	if !ok {
		return files.Download{}, files.ErrNotFound
	}
	if m.ScanStatus != files.ScanClean && m.ScanStatus != files.ScanSkipped {
		return files.Download{}, files.ErrNotClean
	}
	f.ttls = append(f.ttls, ttl)
	return files.Download{URL: "https://files.koro.test/" + id.String(), ExpiresAt: f.clk.Now().Add(ttl)}, nil
}

func (f *fakeFiles) remove(_ context.Context, _ authz.Actor, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	delete(f.metas, id)
	return nil
}

// eduEnv is an identity module with the fake directory and files.
type eduEnv struct {
	*env
	files *fakeFiles
}

func newEduEnv(t *testing.T, opts ...func(*identity.Deps)) *eduEnv {
	t.Helper()
	ff := &fakeFiles{metas: map[uuid.UUID]files.FileMeta{}}
	e := newEnv(t, append([]func(*identity.Deps){func(d *identity.Deps) {
		d.Domains, d.Files, d.RemoveFile = fakeDomains, ff, ff.remove
		d.UniversityNames = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
			return map[uuid.UUID]string{eduUni: "Koro Üniversitesi"}, nil
		}
	}}, opts...)...)
	ff.clk = e.clk
	return &eduEnv{env: e, files: ff}
}

// student is an active account with its actor (no student status yet).
func (e *eduEnv) student(t *testing.T, email string) (identity.User, authz.Actor) {
	t.Helper()
	u := e.activeUser(t, email)
	return u, authz.Actor{Kind: authz.KindUser, UserID: u.ID, SessionID: uuid.NewV7(), StudentStatus: authz.StudentNone}
}

// staff is a platform staff actor with an account (MFA-verified session).
func (e *eduEnv) staff(t *testing.T, email string, role authz.PlatformRole) authz.Actor {
	t.Helper()
	u := e.activeUser(t, email)
	return authz.Actor{Kind: authz.KindUser, UserID: u.ID, SessionID: uuid.NewV7(), PlatformRoles: []authz.PlatformRole{role},
		MFAVerified: true, StudentStatus: authz.StudentNone}
}

// verifyByEmail runs start + confirm and returns the token used.
func (e *eduEnv) verifyByEmail(t *testing.T, a authz.Actor, address string) string {
	t.Helper()
	if err := e.svc.StartEduVerification(t.Context(), a, address); err != nil {
		t.Fatalf("start: %v", err)
	}
	tok := e.lastToken(t, "identity.edu_verify")
	if err := e.svc.ConfirmEduVerification(t.Context(), authz.Anonymous(), tok); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return tok
}

func (e *eduEnv) eduRows(t *testing.T, userID uuid.UUID) []identity.EduRecord {
	t.Helper()
	rows, err := e.store.EduVerificationsOf(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func (e *eduEnv) eduEvents(t *testing.T, topic string) []domainevent.Envelope {
	t.Helper()
	var out []domainevent.Envelope
	for _, ev := range e.out.Events() {
		if ev.Topic == topic {
			out = append(out, ev)
		}
	}
	return out
}

func decodeVerified(t *testing.T, ev domainevent.Envelope) domainevent.UserEduVerified {
	t.Helper()
	p, err := domainevent.Decode[domainevent.UserEduVerified](ev)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStartEdu_UnknownDomain_422(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	_, a := e.student(t, "ada@example.com")
	for _, addr := range []string{"ada@bilinmeyen.edu.tr", "ada@gmail.com", "ada@edu.tr", "ada@uni.edu.tr.evil.com"} {
		err := e.svc.StartEduVerification(t.Context(), a, addr)
		wantErr(t, err, identity.ErrEduDomainUnknown)
		wantStatus(t, err, http.StatusUnprocessableEntity)
		pe, _ := errors.AsType[*problem.Error](err)
		if fe := pe.Fields(); len(fe) != 1 || fe[0].Location != "body.edu_email" || strings.Contains(fe[0].Message, "ada@") {
			t.Fatalf("%s: field errors %+v (the address must not be echoed)", addr, fe)
		}
	}
	// Nothing was issued: no token, no e-mail, no audit of a start.
	e.deliver(t)
	if n := len(e.audit.of(string(authz.IdentityEduVerificationStart))); n != 0 {
		t.Fatalf("%d start audits for unknown domains", n)
	}
	for _, m := range e.mail.sent() {
		if m.Template == "identity.edu_verify" {
			t.Fatal("a verification e-mail went to an unknown domain")
		}
	}
	// A subdomain of a known university resolves (öğrenci adresleri).
	if err := e.svc.StartEduVerification(t.Context(), a, "ada@ogr.uni.edu.tr"); err != nil {
		t.Fatalf("subdomain: %v", err)
	}
	// Anonymous callers cannot start (O).
	wantErr(t, e.svc.StartEduVerification(t.Context(), authz.Anonymous(), "x@uni.edu.tr"), authz.ErrForbidden)
}

func TestStartEdu_NilResolver_422(t *testing.T) {
	t.Parallel()
	// directory is not wired (hooks.domainResolver nil): no domain is known,
	// so nobody becomes a student by e-mail; the document path stays open.
	e := newEduEnv(t, func(d *identity.Deps) { d.Domains = nil })
	_, a := e.student(t, "ada@example.com")
	err := e.svc.StartEduVerification(t.Context(), a, "ada@ogr.uni.edu.tr")
	wantErr(t, err, identity.ErrEduDomainUnknown)
	wantStatus(t, err, http.StatusUnprocessableEntity)
	if len(e.eduRows(t, a.UserID)) != 0 {
		t.Fatal("a verification row exists")
	}
	doc := e.files.add(a.UserID, files.PurposeEduDocument, files.ScanClean)
	if _, err := e.svc.SubmitEduDocument(t.Context(), a, identity.EduDocumentSubmission{UniversityID: eduUni, FileID: doc}); err != nil {
		t.Fatalf("document path without a resolver: %v", err)
	}
}

func TestConfirmEdu_SetsVerifiedWithOneYearExpiry(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	u, a := e.student(t, "ada@example.com")
	ctx := t.Context()
	if err := e.svc.StartEduVerification(ctx, a, "Ada.Yilmaz@Ogr.Uni.edu.tr"); err != nil {
		t.Fatal(err)
	}
	if len(e.eduRows(t, u.ID)) != 0 {
		t.Fatal("start wrote a verification row (an address must not be reservable before it is proven)")
	}
	tok := e.lastToken(t, "identity.edu_verify")
	last := e.mail.sent()[len(e.mail.sent())-1]
	if last.ToEmail != "Ada.Yilmaz@Ogr.Uni.edu.tr" || !strings.Contains(last.Data["link"].(string), "/auth/verify-edu#token=") {
		t.Fatalf("verification e-mail = to %q link %v", last.ToEmail, last.Data["link"])
	}
	e.clk.Advance(time.Hour)
	if err := e.svc.ConfirmEduVerification(ctx, authz.Anonymous(), tok); err != nil {
		t.Fatal(err)
	}
	rows := e.eduRows(t, u.ID)
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	v := rows[0]
	now := e.clk.Now().UTC().Truncate(time.Microsecond)
	if v.Status != identity.EduVerified || v.Method != identity.EduMethodEmail || *v.UniversityID != eduUni ||
		!v.VerifiedAt.Equal(now) || !v.ExpiresAt.Equal(now.AddDate(1, 0, 0)) || v.EmailDomain != "ogr.uni.edu.tr" ||
		v.EduEmailNormalized != "ada.yilmaz@ogr.uni.edu.tr" {
		t.Fatalf("verification = %+v", v)
	}
	st, err := e.mod.Students.StudentStatus(ctx, u.ID)
	if err != nil || st.Status != authz.StudentVerified || st.Active.UniversityID != eduUni {
		t.Fatalf("student status = %+v, %v", st, err)
	}
	if got, ok, _ := e.mod.Students.Active(ctx, u.ID); !ok || !got.ExpiresAt.Equal(*v.ExpiresAt) {
		t.Fatalf("Active = %+v %v", got, ok)
	}
	evs := e.eduEvents(t, domainevent.TopicUserEduVerified)
	if len(evs) != 1 {
		t.Fatalf("user.edu_verified events: %d", len(evs))
	}
	if p := decodeVerified(t, evs[0]); p.UserID != u.ID || p.UniversityID != eduUni || p.Renewal || !p.ExpiresAt.Equal(*v.ExpiresAt) {
		t.Fatalf("event = %+v", p)
	}
	if n := len(e.audit.of(string(authz.IdentityEduVerificationConfirm))); n != 1 {
		t.Fatalf("confirm audits %d", n)
	}
	// Single use.
	wantErr(t, e.svc.ConfirmEduVerification(ctx, authz.Anonymous(), tok), identity.ErrInvalidToken)
	// Expired after 24 hours.
	if err := e.svc.StartEduVerification(ctx, a, "ada@uni.edu.tr"); err != nil {
		t.Fatal(err)
	}
	late := e.lastToken(t, "identity.edu_verify")
	e.clk.Advance(25 * time.Hour)
	wantErr(t, e.svc.ConfirmEduVerification(ctx, authz.Anonymous(), late), identity.ErrInvalidToken)
}

func TestConfirmEdu_RenewalSupersedesOld(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	u, a := e.student(t, "ada@example.com")
	ctx := t.Context()
	e.verifyByEmail(t, a, "ada@uni.edu.tr")
	first := e.eduRows(t, u.ID)[0]
	// 11 months later: a pending document and a renewal by e-mail.
	e.clk.Advance(330 * 24 * time.Hour)
	doc := e.files.add(u.ID, files.PurposeEduDocument, files.ScanClean)
	if _, err := e.svc.SubmitEduDocument(ctx, a, identity.EduDocumentSubmission{UniversityID: eduUni, FileID: doc}); err != nil {
		t.Fatal(err)
	}
	e.verifyByEmail(t, a, "ada@uni.edu.tr") // the same address renews (it is this user's)
	rows := e.eduRows(t, u.ID)
	byID := map[uuid.UUID]identity.EduRecord{}
	verified := 0
	for _, r := range rows {
		byID[r.ID] = r
		if r.Status == identity.EduVerified {
			verified++
		}
	}
	if verified != 1 || byID[first.ID].Status != identity.EduSuperseded {
		t.Fatalf("rows after the renewal %+v", rows)
	}
	for _, r := range rows {
		if r.Method == identity.EduMethodDocument && r.Status != identity.EduSuperseded {
			t.Fatalf("the pending document stayed in the queue: %+v", r)
		}
	}
	newest := rows[0]
	if !newest.ExpiresAt.After(*first.ExpiresAt) {
		t.Fatalf("renewal expiry %v not after %v", newest.ExpiresAt, first.ExpiresAt)
	}
	evs := e.eduEvents(t, domainevent.TopicUserEduVerified)
	if len(evs) != 2 || !decodeVerified(t, evs[1]).Renewal || decodeVerified(t, evs[0]).Renewal {
		t.Fatalf("events %d (renewal flag on the second only)", len(evs))
	}
	// Another account cannot take the address while it verifies this one:
	// the start answers like a free address and sends nothing.
	_, b := e.student(t, "bora@example.com")
	sent := len(e.mail.sent())
	if err := e.svc.StartEduVerification(ctx, b, "ADA@uni.edu.tr"); err != nil {
		t.Fatalf("taken address start = %v, want nil (202)", err)
	}
	e.deliver(t)
	if len(e.mail.sent()) != sent {
		t.Fatal("a token went to an address that verifies another account")
	}
	if a := e.audit.of(string(authz.IdentityEduVerificationStart)); a[len(a)-1].Metadata["result"] != "address_taken" {
		t.Fatalf("start audit %+v", a[len(a)-1].Metadata)
	}
}

func TestEduConfirm_AddressTakenMeanwhile_409(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	_, a := e.student(t, "ada@example.com")
	_, b := e.student(t, "bora@example.com")
	ctx := t.Context()
	if err := e.svc.StartEduVerification(ctx, b, "ortak@uni.edu.tr"); err != nil {
		t.Fatal(err)
	}
	tokB := e.lastToken(t, "identity.edu_verify")
	e.verifyByEmail(t, a, "ortak@uni.edu.tr")
	err := e.svc.ConfirmEduVerification(ctx, authz.Anonymous(), tokB)
	wantErr(t, err, identity.ErrEduEmailTaken)
	wantStatus(t, err, http.StatusConflict)
}

func TestSubmitEduDocument_QueuesForAdmin(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	u, a := e.student(t, "ada@example.com")
	other, _ := e.student(t, "bora@example.com")
	ctx := t.Context()
	sub := func(file uuid.UUID) (identity.EduView, error) {
		return e.svc.SubmitEduDocument(ctx, a, identity.EduDocumentSubmission{UniversityID: eduUni, FileID: file})
	}
	// Someone else's upload, another purpose, an unknown file, an infected one: one answer.
	for name, f := range map[string]uuid.UUID{
		"other user's": e.files.add(other.ID, files.PurposeEduDocument, files.ScanClean),
		"a CV":         e.files.add(u.ID, files.PurposeCV, files.ScanClean),
		"unknown":      uuid.NewV7(),
		"infected":     e.files.add(u.ID, files.PurposeEduDocument, files.ScanInfected),
	} {
		_, err := sub(f)
		wantErr(t, err, identity.ErrInvalidEduDocument)
		wantStatus(t, err, http.StatusUnprocessableEntity)
		_ = name
	}
	doc := e.files.add(u.ID, files.PurposeEduDocument, files.ScanPending) // the scan may still run
	v, err := sub(doc)
	if err != nil {
		t.Fatal(err)
	}
	if v.Record.Status != identity.EduPending || v.Record.Method != identity.EduMethodDocument || *v.Record.DocumentFileID != doc ||
		v.UniversityName != "Koro Üniversitesi" {
		t.Fatalf("submission = %+v", v)
	}
	if m, _ := e.files.Get(ctx, doc); m.AttachedModule != "identity" || m.AttachedEntityID != v.Record.ID {
		t.Fatalf("document attached to %q %v", m.AttachedModule, m.AttachedEntityID)
	}
	if e.files.attachTx[0] == nil {
		t.Fatal("the document was attached outside the verification's transaction")
	}
	// One document in review at a time.
	_, err = sub(e.files.add(u.ID, files.PurposeEduDocument, files.ScanClean))
	wantErr(t, err, identity.ErrEduPending)
	wantStatus(t, err, http.StatusConflict)
	// Pending is not a student (03 §2.3).
	if st, _ := e.mod.Students.StudentStatus(ctx, u.ID); st.Status != authz.StudentNone {
		t.Fatalf("pending document gives status %s", st.Status)
	}
	my, err := e.svc.MyEduVerification(ctx, a)
	if err != nil || my.Status != "pending" || my.Current.Record.ID != v.Record.ID {
		t.Fatalf("my verification = %+v, %v", my, err)
	}
	// The queue of support shows it with the applicant's name, oldest first.
	sup := e.staff(t, "destek@example.com", authz.PlatformSupport)
	p, err := e.svc.EduQueue(ctx, sup, identity.EduQuery{})
	if err != nil || len(p.Items) != 1 || p.Items[0].Record.ID != v.Record.ID || p.Items[0].User == nil || p.Items[0].User.FirstName != "Ada" {
		t.Fatalf("queue = %+v, %v", p, err)
	}
	// Students and dpo do not see the queue.
	wantErr(t, func() error { _, err := e.svc.EduQueue(ctx, a, identity.EduQuery{}); return err }(), authz.ErrForbidden)
	dpo := e.staff(t, "dpo@example.com", authz.PlatformDPO)
	wantErr(t, func() error { _, err := e.svc.EduQueue(ctx, dpo, identity.EduQuery{}); return err }(), authz.ErrForbidden)
	if n := len(e.audit.of(string(authz.IdentityEduVerificationSubmitDocument))); n != 1 {
		t.Fatalf("submit audits %d", n)
	}
}

func TestAdminDecide_ApproveEmitsEduVerified(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	u, a := e.student(t, "ada@example.com")
	ctx := t.Context()
	doc := e.files.add(u.ID, files.PurposeEduDocument, files.ScanPending)
	v, err := e.svc.SubmitEduDocument(ctx, a, identity.EduDocumentSubmission{UniversityID: eduUni, FileID: doc})
	if err != nil {
		t.Fatal(err)
	}
	pad := e.staff(t, "admin@example.com", authz.PlatformAdmin)
	uni := eduUni
	approve := identity.EduDecision{Decision: identity.EduVerified, UniversityID: &uni, ReviewNote: "Açıköğretim belgesi"}
	// The scan has not cleared the document yet: no approval.
	_, err = e.svc.DecideEduVerification(ctx, pad, v.Record.ID, approve)
	wantErr(t, err, files.ErrNotClean)
	e.files.mu.Lock()
	m := e.files.metas[doc]
	m.ScanStatus = files.ScanClean
	e.files.metas[doc] = m
	e.files.mu.Unlock()
	// Approval needs the university.
	_, err = e.svc.DecideEduVerification(ctx, pad, v.Record.ID, identity.EduDecision{Decision: identity.EduVerified})
	wantErr(t, err, identity.ErrUniversityUnknown)
	got, err := e.svc.DecideEduVerification(ctx, pad, v.Record.ID, approve)
	if err != nil {
		t.Fatal(err)
	}
	now := e.clk.Now().UTC().Truncate(time.Microsecond)
	r := got.Record
	if r.Status != identity.EduVerified || *r.ReviewedBy != pad.UserID || !r.ExpiresAt.Equal(now.AddDate(1, 0, 0)) || got.User == nil {
		t.Fatalf("decision = %+v", got)
	}
	evs := e.eduEvents(t, domainevent.TopicUserEduVerified)
	if len(evs) != 1 || decodeVerified(t, evs[0]).UserID != u.ID || decodeVerified(t, evs[0]).Renewal {
		t.Fatalf("user.edu_verified events %d", len(evs))
	}
	if st, _ := e.mod.Students.StudentStatus(ctx, u.ID); st.Status != authz.StudentVerified {
		t.Fatalf("status after approval %s", st.Status)
	}
	if au := e.audit.of(string(authz.IdentityEduVerificationReview)); len(au) != 1 || au[0].Metadata["decision"] != "verified" {
		t.Fatalf("review audits %+v", au)
	}
	// The applicant is told (A).
	notices := 0
	for _, x := range e.q.Tasks() {
		if x.Type == identity.SendEduNoticeTaskName && strings.Contains(string(x.Payload), "document_approved") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("approval notices %d", notices)
	}
	// A decided verification cannot be decided again.
	_, err = e.svc.DecideEduVerification(ctx, pad, v.Record.ID, identity.EduDecision{Decision: identity.EduRejected})
	wantErr(t, err, identity.ErrEduInvalidState)
	wantStatus(t, err, http.StatusConflict)

	// Y6: nobody decides their own document; an unknown ID is 404 for staff only.
	padUser, _ := e.store.User(ctx, pad.UserID)
	own := e.files.add(padUser.ID, files.PurposeEduDocument, files.ScanClean)
	ownV, err := e.svc.SubmitEduDocument(ctx, pad, identity.EduDocumentSubmission{UniversityID: eduUni, FileID: own})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.DecideEduVerification(ctx, pad, ownV.Record.ID, approve)
	wantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.DecideEduVerification(ctx, pad, uuid.NewV7(), approve)
	wantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.DecideEduVerification(ctx, a, uuid.NewV7(), approve)
	wantErr(t, err, authz.ErrForbidden)
	// A rejection keeps the note for the applicant (and only then shows it).
	sup := e.staff(t, "destek@example.com", authz.PlatformSupport)
	rej, err := e.svc.DecideEduVerification(ctx, sup, ownV.Record.ID, identity.EduDecision{Decision: identity.EduRejected, ReviewNote: "Belge okunmuyor"})
	if err != nil || rej.Record.Status != identity.EduRejected || rej.Record.ReviewNote != "Belge okunmuyor" {
		t.Fatalf("rejection = %+v, %v", rej, err)
	}
	my, _ := e.svc.MyEduVerification(ctx, pad)
	if my.Status != "rejected" || my.Current.Record.ReviewNote != "Belge okunmuyor" {
		t.Fatalf("applicant view after the rejection %+v", my)
	}
}

func TestAdminGetEduDocument_Audited(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	u, a := e.student(t, "ada@example.com")
	ctx := t.Context()
	doc := e.files.add(u.ID, files.PurposeEduDocument, files.ScanPending)
	v, err := e.svc.SubmitEduDocument(ctx, a, identity.EduDocumentSubmission{UniversityID: eduUni, FileID: doc})
	if err != nil {
		t.Fatal(err)
	}
	sup := e.staff(t, "destek@example.com", authz.PlatformSupport)
	// Not cleared by the scan yet: 409, nothing handed out, nothing audited as a download.
	_, err = e.svc.EduDocument(ctx, sup, v.Record.ID)
	wantErr(t, err, files.ErrNotClean)
	if n := len(e.audit.of(string(authz.IdentityEduVerificationReview))); n != 0 {
		t.Fatalf("%d audits for a refused download", n)
	}
	e.files.mu.Lock()
	m := e.files.metas[doc]
	m.ScanStatus = files.ScanClean
	e.files.metas[doc] = m
	e.files.mu.Unlock()
	for i := range 2 { // every call is audited
		l, err := e.svc.EduDocument(ctx, sup, v.Record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if l.URL == "" || l.ExpiresAt.Sub(e.clk.Now()) > 5*time.Minute {
			t.Fatalf("link %+v", l)
		}
		au := e.audit.of(string(authz.IdentityEduVerificationReview))
		if len(au) != i+1 {
			t.Fatalf("download %d: %d audits", i+1, len(au))
		}
		last := au[len(au)-1]
		if last.Metadata["operation"] != "document_download" || last.Metadata["file_id"] != doc.String() ||
			last.Metadata["subject_user_id"] != u.ID.String() || *last.ResourceID != v.Record.ID || last.Outcome != audit.Success {
			t.Fatalf("audit %+v", last)
		}
	}
	for _, ttl := range e.files.ttls {
		if ttl <= 0 || ttl > 5*time.Minute {
			t.Fatalf("presign ttl %v (≤ 5 minutes)", ttl)
		}
	}
	// The applicant (no staff role) gets 403; staff without the action too.
	_, err = e.svc.EduDocument(ctx, a, v.Record.ID)
	wantErr(t, err, authz.ErrForbidden)
	dpo := e.staff(t, "dpo@example.com", authz.PlatformDPO)
	_, err = e.svc.EduDocument(ctx, dpo, v.Record.ID)
	wantErr(t, err, authz.ErrForbidden)
	// An e-mail verification has no document: 404.
	_, b := e.student(t, "bora@example.com")
	e.verifyByEmail(t, b, "bora@uni.edu.tr")
	emailRow := e.eduRows(t, b.UserID)[0]
	_, err = e.svc.EduDocument(ctx, sup, emailRow.ID)
	wantErr(t, err, problem.ErrNotFound)
}

func TestEduExpire_EmitsLapsedEvent(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	u, a := e.student(t, "ada@example.com")
	_, b := e.student(t, "bora@example.com")
	ctx := t.Context()
	e.verifyByEmail(t, a, "ada@uni.edu.tr")
	e.clk.Advance(200 * 24 * time.Hour)
	e.verifyByEmail(t, b, "bora@uni.edu.tr")
	job := eduJob(t, e.env, identity.EduExpireJobName)
	if job.Cron != "30 0 * * *" {
		t.Fatalf("edu_expire cron %q (00:30)", job.Cron)
	}
	// One year and a day later: only Ada's verification ran out.
	e.clk.Advance(166 * 24 * time.Hour)
	// Before the job runs the actor is already lapsed (expires_at is checked on every read).
	if st, _ := e.mod.Students.StudentStatus(ctx, u.ID); st.Status != authz.StudentLapsed {
		t.Fatalf("status past the expiry, before the job: %s", st.Status)
	}
	if err := job.Handle(workerCtx(t)); err != nil {
		t.Fatal(err)
	}
	rows := e.eduRows(t, u.ID)
	if rows[0].Status != identity.EduExpired {
		t.Fatalf("row after the job %+v", rows[0])
	}
	if rows := e.eduRows(t, b.UserID); rows[0].Status != identity.EduVerified {
		t.Fatalf("Bora's verification %+v", rows[0])
	}
	evs := e.eduEvents(t, domainevent.TopicUserEduVerificationLapsed)
	if len(evs) != 1 {
		t.Fatalf("lapsed events %d", len(evs))
	}
	p, err := domainevent.Decode[domainevent.UserEduVerificationLapsed](evs[0])
	if err != nil || p.UserID != u.ID || p.VerificationID != rows[0].ID {
		t.Fatalf("lapsed event %+v %v", p, err)
	}
	if au := e.audit.of(string(authz.IdentityEduVerificationExpire)); len(au) != 1 || au[0].SystemJob != identity.EduExpireJobName {
		t.Fatalf("expire audits %+v", au)
	}
	if st, _ := e.mod.Students.StudentStatus(ctx, u.ID); st.Status != authz.StudentLapsed {
		t.Fatalf("status after the job %s", st.Status)
	}
	my, _ := e.svc.MyEduVerification(ctx, a)
	if my.Status != "lapsed" {
		t.Fatalf("my verification after the expiry: %s", my.Status)
	}
	// Idempotent: a second run lapses nothing again.
	if err := job.Handle(workerCtx(t)); err != nil {
		t.Fatal(err)
	}
	if n := len(e.eduEvents(t, domainevent.TopicUserEduVerificationLapsed)); n != 1 {
		t.Fatalf("lapsed events after a rerun %d", n)
	}
	// The student is told once; a renewal restores the status.
	e.verifyByEmail(t, a, "ada@uni.edu.tr")
	if st, _ := e.mod.Students.StudentStatus(ctx, u.ID); st.Status != authz.StudentVerified {
		t.Fatalf("status after re-verifying %s", st.Status)
	}
}

func eduJob(t *testing.T, e *env, name string) jobUnderTest {
	t.Helper()
	for _, j := range e.mod.Jobs(nil, nil) {
		if j.Name == name {
			return jobUnderTest{Cron: j.Cron, Handle: j.Handle}
		}
	}
	t.Fatalf("no job %s", name)
	return jobUnderTest{}
}

type jobUnderTest struct {
	Cron   string
	Handle func(context.Context) error
}

func TestRenewalReminders_30_7_1Days(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	u, a := e.student(t, "ada@example.com")
	// Verified at 2026-10-03 15:00 Istanbul: the year ends 2027-10-03 15:00.
	e.clk.Set(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	e.verifyByEmail(t, a, "ada@uni.edu.tr")
	job := eduJob(t, e.env, identity.EduRenewalRemindersJobName)
	if job.Cron != "0 10 * * *" {
		t.Fatalf("reminder cron %q (10:00)", job.Cron)
	}
	reminders := func() []string {
		var out []string
		for _, x := range e.q.Tasks() {
			if x.Type != identity.SendEduNoticeTaskName {
				continue
			}
			var n struct {
				Kind     string `json:"kind"`
				DaysLeft int    `json:"days_left"`
			}
			if err := json.Unmarshal(x.Payload, &n); err != nil {
				t.Fatal(err)
			}
			if n.Kind == "renewal_reminder" {
				out = append(out, strconv.Itoa(n.DaysLeft))
			}
		}
		return out
	}
	runAt := func(day time.Time) {
		e.clk.Set(day) // 10:00 Istanbul = 07:00 UTC
		if err := job.Handle(workerCtx(t)); err != nil {
			t.Fatal(err)
		}
	}
	istanbul := clock.Istanbul()
	at := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 10, 0, 0, 0, istanbul) }
	runAt(at(2027, 9, 2))  // 31 days left: nothing
	runAt(at(2027, 9, 3))  // 30
	runAt(at(2027, 9, 3))  // the same day again: deduplicated
	runAt(at(2027, 9, 20)) // nothing
	runAt(at(2027, 9, 26)) // 7
	runAt(at(2027, 10, 2)) // 1
	runAt(at(2027, 10, 3)) // the last day: no "0 days" reminder
	if got, want := reminders(), []string{"30", "7", "1"}; !slices.Equal(got, want) {
		t.Fatalf("reminders %v, want %v (days 30, 7, 1)", got, want)
	}
	// The worker sends them to the account address, with the days left.
	task := e.mod.Tasks()
	var send func(context.Context, []byte) error
	for _, x := range task {
		if x.Name == identity.SendEduNoticeTaskName {
			send = x.Handle
		}
	}
	for _, x := range e.q.Tasks() {
		if x.Type == identity.SendEduNoticeTaskName && strings.Contains(string(x.Payload), "renewal_reminder") {
			if err := send(workerCtx(t), x.Payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	var days []string
	for _, m := range e.mail.sent() {
		if m.Template == "identity.edu_notice" {
			if m.ToEmail != "ada@example.com" || m.Category != "transactional" || m.UserID == nil || *m.UserID != u.ID {
				t.Fatalf("reminder e-mail %+v", m)
			}
			days = append(days, m.Data["days_left"].(string))
		}
	}
	if !slices.Equal(days, []string{"30", "7", "1"}) {
		t.Fatalf("reminder e-mails %v", days)
	}
	// A renewed verification is not reminded: the old row is superseded.
	e.clk.Set(at(2027, 10, 2))
	e.verifyByEmail(t, a, "ada@uni.edu.tr")
	e.q.Reset()
	runAt(at(2027, 10, 2).Add(time.Hour))
	if got := reminders(); len(got) != 0 {
		t.Fatalf("reminders after the renewal %v", got)
	}
}

func TestEduErase_DeletesVerificationsAndDocuments(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	u, a := e.student(t, "ada@example.com")
	ctx := t.Context()
	e.verifyByEmail(t, a, "ada@uni.edu.tr")
	doc := e.files.add(u.ID, files.PurposeEduDocument, files.ScanClean)
	if _, err := e.svc.SubmitEduDocument(ctx, a, identity.EduDocumentSubmission{UniversityID: eduUni, FileID: doc}); err != nil {
		t.Fatal(err)
	}
	sec, err := e.mod.Privacy.Export(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range sec.Items {
		if it["type"] == "student_verification" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("export has %d student verifications", n)
	}
	if _, err := e.mod.Privacy.Erase(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if rows := e.eduRows(t, u.ID); len(rows) != 0 {
		t.Fatalf("rows after the erasure %+v", rows)
	}
	if !slices.Contains(e.files.removed, doc) {
		t.Fatal("the student document was not deleted")
	}
	if st, _ := e.mod.Students.StudentStatus(ctx, u.ID); st.Status != authz.StudentNone {
		t.Fatalf("status after the erasure %s", st.Status)
	}
}

func TestEduFileRejected_WithdrawsPendingDocument(t *testing.T) {
	t.Parallel()
	e := newEduEnv(t)
	u, a := e.student(t, "ada@example.com")
	ctx := t.Context()
	doc := e.files.add(u.ID, files.PurposeEduDocument, files.ScanPending)
	v, err := e.svc.SubmitEduDocument(ctx, a, identity.EduDocumentSubmission{UniversityID: eduUni, FileID: doc})
	if err != nil {
		t.Fatal(err)
	}
	uploader, entity := u.ID, v.Record.ID
	env, err := domainevent.New(domainevent.FileRejected{FileID: doc, UploaderID: &uploader, Purpose: "edu_document",
		Reason: "infected", AttachedModule: "identity", AttachedEntityID: &entity}, "file", doc)
	if err != nil {
		t.Fatal(err)
	}
	e.mod.SubscribeFileRejected(e.subs)
	subs := e.subs.For(domainevent.TopicFileRejected)
	if len(subs) != 1 || subs[0].Name != identity.RemoveRejectedDocumentTaskName {
		t.Fatalf("file.rejected subscriptions %+v", subs)
	}
	b, _ := json.Marshal(env)
	for range 2 { // at-least-once delivery
		if err := subs[0].HandleTask(workerCtx(t), b); err != nil {
			t.Fatal(err)
		}
	}
	if rows := e.eduRows(t, u.ID); len(rows) != 0 {
		t.Fatalf("the pending verification stayed: %+v", rows)
	}
	if !slices.Contains(e.files.removed, doc) {
		t.Fatal("the rejected document was not deleted")
	}
	// The student can submit a new document at once.
	if _, err := e.svc.SubmitEduDocument(ctx, a, identity.EduDocumentSubmission{UniversityID: eduUni,
		FileID: e.files.add(u.ID, files.PurposeEduDocument, files.ScanClean)}); err != nil {
		t.Fatal(err)
	}
}
