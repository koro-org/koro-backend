package students_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// cv uploads a cv file of a and adds it as a CV.
func (e *env) cv(t *testing.T, a authz.Actor, makeCurrent bool) students.CVView {
	t.Helper()
	v, err := e.svc.CreateMyCV(t.Context(), a, e.files.upload(a.UserID, files.PurposeCV), makeCurrent)
	if err != nil {
		t.Fatalf("CreateMyCV: %v", err)
	}
	return v
}

func currentOf(t *testing.T, e *env, a authz.Actor) []uuid.UUID {
	t.Helper()
	vs, err := e.svc.ListMyCVs(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	var out []uuid.UUID
	for _, v := range vs {
		if v.CV.IsCurrent {
			out = append(out, v.CV.ID)
		}
	}
	return out
}

// TestCreateCv_FourthCv_409 (02 §9.6, 04 §2.4): at most 3 CVs; the fourth
// is 409 students.cv_limit and changes nothing (no row, no attachment).
// The first CV is current and completes the cv section; the file is
// attached to the CV row (files.Attach, purpose cv).
func TestCreateCv_FourthCv_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	first := e.cv(t, a, false)
	if !first.CV.IsCurrent || first.FileName != "cv.pdf" {
		t.Fatalf("first CV = %+v, want current with its file name", first)
	}
	if owner := e.files.attached[first.CV.FileID]; owner != (files.Owner{Module: "students", EntityID: first.CV.ID, Purpose: files.PurposeCV}) {
		t.Fatalf("attached to %+v", owner)
	}
	if p := e.profile(t, a.UserID); !p.Completed(students.SectionCV) {
		t.Fatalf("cv section not completed: %v", p.CompletedSections)
	}
	e.cv(t, a, false)
	e.cv(t, a, false)
	fourth := e.files.upload(a.UserID, files.PurposeCV)
	_, err := e.svc.CreateMyCV(ctx, a, fourth, true)
	wantCode(t, err, "students.cv_limit")
	var pe interface{ Status() int }
	if !errors.As(err, &pe) || pe.Status() != 409 {
		t.Fatalf("fourth CV status = %v", err)
	}
	if vs, _ := e.svc.ListMyCVs(ctx, a); len(vs) != 3 {
		t.Fatalf("CVs after the refused fourth = %d", len(vs))
	}
	if _, ok := e.files.attached[fourth]; ok {
		t.Fatal("the refused CV's file was attached")
	}
	if got := len(e.audit.of(string(authz.StudentsCvManage))); got != 3 {
		t.Fatalf("cv.manage audit records = %d, want 3", got)
	}
}

// TestCreateCv_OwnPdfUploadOnly: the file must be a cv upload of the
// caller (another user's upload, an avatar, an unknown file: 422; nothing
// is written); an account without a student verification is 403
// students.verification_required before any file is read.
func TestCreateCv_OwnPdfUploadOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a, b := e.student(t, e.uniA, false), e.student(t, e.uniA, false)
	for name, f := range map[string]uuid.UUID{
		"another user's cv": e.files.upload(b.UserID, files.PurposeCV),
		"own avatar":        e.files.upload(a.UserID, files.PurposeAvatar),
		"unknown":           uuid.NewV7(),
	} {
		_, err := e.svc.CreateMyCV(ctx, a, f, false)
		var pe *problem.Error
		if !errors.As(err, &pe) || pe.Code() != "validation_failed" {
			t.Fatalf("%s: %v, want 422 validation_failed", name, err)
		}
	}
	if vs, _ := e.svc.ListMyCVs(ctx, a); len(vs) != 0 {
		t.Fatalf("refused uploads wrote CVs: %v", vs)
	}
	_, err := e.svc.CreateMyCV(ctx, e.unverified(), e.files.upload(a.UserID, files.PurposeCV), false)
	wantCode(t, err, "students.verification_required")
}

// TestSetCurrentCv_SingleCurrent (02 §9.6 student_cvs_current_uq): exactly
// one current CV after every change; make_current on create moves the
// mark; setting the current one again writes nothing; another user's CV
// is 404.
func TestSetCurrentCv_SingleCurrent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a, b := e.student(t, e.uniA, false), e.student(t, e.uniB, false)
	c1 := e.cv(t, a, false)
	c2 := e.cv(t, a, false)
	if got := currentOf(t, e, a); !slices.Equal(got, []uuid.UUID{c1.CV.ID}) {
		t.Fatalf("current after two = %v, want the first", got)
	}
	c3 := e.cv(t, a, true)
	if got := currentOf(t, e, a); !slices.Equal(got, []uuid.UUID{c3.CV.ID}) {
		t.Fatalf("current after make_current = %v", got)
	}
	v, err := e.svc.SetMyCurrentCV(ctx, a, c2.CV.ID)
	if err != nil || !v.CV.IsCurrent || v.FileName != "cv.pdf" {
		t.Fatalf("SetMyCurrentCV = %+v, %v", v, err)
	}
	if got := currentOf(t, e, a); !slices.Equal(got, []uuid.UUID{c2.CV.ID}) {
		t.Fatalf("current = %v, want only %v", got, c2.CV.ID)
	}
	before := len(e.audit.of(string(authz.StudentsCvManage)))
	if _, err := e.svc.SetMyCurrentCV(ctx, a, c2.CV.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.audit.of(string(authz.StudentsCvManage))) != before {
		t.Fatal("an unchanged current CV was audited as a write")
	}
	if _, err := e.svc.SetMyCurrentCV(ctx, b, c1.CV.ID); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("another user's CV: %v, want 404", err)
	}
	if got := currentOf(t, e, a); !slices.Equal(got, []uuid.UUID{c2.CV.ID}) {
		t.Fatalf("another user changed the current CV: %v", got)
	}
}

// TestDeleteCv_DeletesFile (02 §9.6: hard delete, the file included): the
// row and its file go in one transaction (files.Delete through the
// remover, after the row); another user's CV is 404 and its file stays; a
// deleted current CV leaves no current one; the last CV clears the cv
// section.
func TestDeleteCv_DeletesFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a, b := e.student(t, e.uniA, false), e.student(t, e.uniA, false)
	c1 := e.cv(t, a, false)
	c2 := e.cv(t, a, false)
	if err := e.svc.DeleteMyCV(ctx, b, c1.CV.ID); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("another user's delete: %v", err)
	}
	if e.files.wasDeleted(c1.CV.FileID) {
		t.Fatal("another user's request deleted the file")
	}
	if err := e.svc.DeleteMyCV(ctx, a, c1.CV.ID); err != nil {
		t.Fatal(err)
	}
	if !e.files.wasDeleted(c1.CV.FileID) {
		t.Fatal("the CV's file was not deleted")
	}
	if e.files.deleteTx[len(e.files.deleteTx)-1] == 0 {
		t.Fatal("the file was deleted outside the CV's transaction")
	}
	if _, err := e.store.GetCV(ctx, a.UserID, c1.CV.ID); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("row survived: %v", err)
	}
	if got := currentOf(t, e, a); len(got) != 0 {
		t.Fatalf("a current CV after deleting the current one: %v", got)
	}
	if !e.profile(t, a.UserID).Completed(students.SectionCV) {
		t.Fatal("cv section cleared while a CV remains")
	}
	if err := e.svc.DeleteMyCV(ctx, a, c2.CV.ID); err != nil {
		t.Fatal(err)
	}
	if e.profile(t, a.UserID).Completed(students.SectionCV) {
		t.Fatal("cv section still completed without a CV")
	}
	if err := e.svc.DeleteMyCV(ctx, a, c2.CV.ID); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

// TestDownloadCv_ShortLivedAndAudited (04 §1.14): the signed URL lives at
// most 5 minutes, every download is audited, a pending scan is 409
// files.not_clean (nothing audited), another user's CV is 404.
func TestDownloadCv_ShortLivedAndAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a, b := e.student(t, e.uniA, false), e.student(t, e.uniA, false)
	c := e.cv(t, a, false)
	for range 2 {
		dl, err := e.svc.DownloadMyCV(ctx, a, c.CV.ID)
		if err != nil || dl.URL == "" {
			t.Fatalf("DownloadMyCV = %+v, %v", dl, err)
		}
	}
	for _, ttl := range e.files.ttls {
		if ttl <= 0 || ttl > 5*time.Minute {
			t.Fatalf("URL lifetime %s, want ≤ 5 min", ttl)
		}
	}
	reads := e.audit.of(string(authz.StudentsCvReadOwn))
	if len(reads) != 2 || reads[0].ResourceType != authz.TypeStudentCV || *reads[0].ResourceID != c.CV.ID ||
		*reads[0].ActorUserID != a.UserID {
		t.Fatalf("download audit = %+v", reads)
	}
	if _, err := e.svc.DownloadMyCV(ctx, b, c.CV.ID); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("another user's download: %v", err)
	}
	pending := e.cv(t, a, false)
	e.files.mu.Lock()
	e.files.notClean[pending.CV.FileID] = true
	e.files.mu.Unlock()
	_, err := e.svc.DownloadMyCV(ctx, a, pending.CV.ID)
	wantCode(t, err, "files.not_clean")
	if len(e.audit.of(string(authz.StudentsCvReadOwn))) != 2 {
		t.Fatal("a refused download was audited")
	}
}

// TestStudents_FileRejected_DropsReference (T-176, T-222): file.rejected
// for a CV attached to students deletes the CV row and the file (the
// current mark and the cv section follow); for an avatar the profile's
// reference is cleared and the file deleted; a file of another module is
// ignored; a second delivery is a no-op. Each removal is audited as the
// system job.
func TestStudents_FileRejected_DropsReference(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	c := e.cv(t, a, true)
	av := e.files.upload(a.UserID, files.PurposeAvatar)
	if _, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Avatar: &av}); err != nil {
		t.Fatal(err)
	}
	cvEntity, userEntity := c.CV.ID, a.UserID
	rejected := domainevent.FileRejected{FileID: c.CV.FileID, UploaderID: &userEntity, Purpose: "cv", Reason: "infected",
		AttachedModule: "students", AttachedEntityID: &cvEntity}
	for range 2 {
		e.deliver(t, students.DropRejectedFileTaskName, rejected, c.CV.FileID)
	}
	if _, err := e.store.GetCV(ctx, a.UserID, c.CV.ID); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("rejected CV kept: %v", err)
	}
	if !e.files.wasDeleted(c.CV.FileID) || e.profile(t, a.UserID).Completed(students.SectionCV) {
		t.Fatal("file not deleted or cv section kept")
	}
	e.deliver(t, students.DropRejectedFileTaskName, domainevent.FileRejected{FileID: av, UploaderID: &userEntity,
		Purpose: "avatar", Reason: "unprocessable", AttachedModule: "students", AttachedEntityID: &userEntity}, av)
	if p := e.profile(t, a.UserID); p.AvatarFileID != nil || !e.files.wasDeleted(av) {
		t.Fatalf("rejected avatar kept: %v", p.AvatarFileID)
	}
	foreign := e.files.upload(a.UserID, files.PurposeCV)
	other := uuid.NewV7()
	e.deliver(t, students.DropRejectedFileTaskName, domainevent.FileRejected{FileID: foreign, Purpose: "cv", Reason: "infected",
		AttachedModule: "identity", AttachedEntityID: &other}, foreign)
	if e.files.wasDeleted(foreign) {
		t.Fatal("another module's file was deleted")
	}
	got := []string{}
	for _, ev := range e.audit.events {
		if ev.SystemJob == students.DropRejectedFileTaskName {
			got = append(got, ev.Action)
			if ev.Metadata[audit.MetaSubjectUserID] != a.UserID.String() {
				t.Errorf("%s without its subject: %v", ev.Action, ev.Metadata)
			}
		}
	}
	if !slices.Equal(got, []string{"students.cv.drop_rejected", "students.profile.drop_rejected_avatar"}) {
		t.Fatalf("system audit = %v", got)
	}
}

// TestPrivacyProvider_CVs: the export lists the CVs (no file bytes, no
// URL); the erasure deletes the rows and their files and is idempotent.
func TestPrivacyProvider_CVs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	c := e.cv(t, a, true)
	sec, err := e.mod.Privacy.Export(ctx, a.UserID)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(sec.Items, func(m map[string]any) bool { return m["record"] == "cv" })
	if i < 0 || sec.Items[i]["cv_id"] != c.CV.ID.String() || sec.Items[i]["is_current"] != true || sec.Items[i]["url"] != nil {
		t.Fatalf("export = %+v", sec.Items)
	}
	res, err := e.mod.Privacy.Erase(ctx, a.UserID)
	if err != nil || res.Deleted == 0 {
		t.Fatalf("Erase = %+v, %v", res, err)
	}
	if cvs, _ := e.store.CVs(ctx, a.UserID); len(cvs) != 0 || !e.files.wasDeleted(c.CV.FileID) {
		t.Fatalf("CVs after erase = %v, file deleted = %v", cvs, e.files.wasDeleted(c.CV.FileID))
	}
	if res, err := e.mod.Privacy.Erase(ctx, a.UserID); err != nil || res.Deleted != 0 {
		t.Fatalf("second Erase = %+v, %v", res, err)
	}
}

// TestHTTP_CVFlow (04 §2.4): createMyCv 201 (an Idempotency-Key replays
// it), listMyCvs, setMyCurrentCv, downloadMyCv {url, expires_at},
// deleteMyCv 204; a malformed or another user's cv_id is 404; a fourth CV
// is 409 students.cv_limit; a non-student is 403.
func TestHTTP_CVFlow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	a := e.student(t, e.uniA, false)
	post := func(fileID uuid.UUID, current bool, key string) *httptest.ResponseRecorder {
		body := `{"file_id":"` + fileID.String() + `","make_current":` + strconv.FormatBool(current) + `}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/me/cvs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer actor-"+a.UserID.String())
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	e.callAs(t, app, a, http.MethodGet, "/api/v1/me/cvs") // registers the actor's token
	f1 := e.files.upload(a.UserID, files.PurposeCV)
	rec := post(f1, false, "cv-key-0001")
	var first students.StudentCVDTO
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &first) != nil || !first.IsCurrent || first.FileName != "cv.pdf" {
		t.Fatalf("createMyCv = %d %s", rec.Code, rec.Body)
	}
	if again := post(f1, false, "cv-key-0001"); again.Code != http.StatusCreated || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay = %d %v", again.Code, again.Header())
	}
	second := post(e.files.upload(a.UserID, files.PurposeCV), false, "")
	var s2 students.StudentCVDTO
	if second.Code != http.StatusCreated || json.Unmarshal(second.Body.Bytes(), &s2) != nil || s2.IsCurrent {
		t.Fatalf("second = %d %s", second.Code, second.Body)
	}
	post(e.files.upload(a.UserID, files.PurposeCV), false, "")
	if r := post(e.files.upload(a.UserID, files.PurposeCV), false, ""); r.Code != http.StatusConflict || code(r) != "students.cv_limit" {
		t.Fatalf("fourth = %d %s", r.Code, r.Body)
	}
	rec = e.callAs(t, app, a, http.MethodGet, "/api/v1/me/cvs")
	var list students.StudentCVList
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Items) != 3 {
		t.Fatalf("listMyCvs = %d %s", rec.Code, rec.Body)
	}
	if rec = e.callAs(t, app, a, http.MethodPost, "/api/v1/me/cvs/"+s2.CVID+"/current"); rec.Code != http.StatusOK {
		t.Fatalf("setMyCurrentCv = %d %s", rec.Code, rec.Body)
	}
	rec = e.callAs(t, app, a, http.MethodGet, "/api/v1/me/cvs/"+s2.CVID+"/download")
	var dl students.StudentCVDownloadDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &dl) != nil || dl.URL == "" || dl.ExpiresAt.After(t0.Add(5*time.Minute)) {
		t.Fatalf("downloadMyCv = %d %s", rec.Code, rec.Body)
	}
	other := e.student(t, e.uniA, false)
	for _, path := range []string{"/api/v1/me/cvs/" + s2.CVID + "/download", "/api/v1/me/cvs/not-a-uuid/download"} {
		if rec = e.callAs(t, app, other, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Fatalf("%s as another user = %d", path, rec.Code)
		}
	}
	if rec = e.callAs(t, app, a, http.MethodDelete, "/api/v1/me/cvs/"+first.CVID); rec.Code != http.StatusNoContent {
		t.Fatalf("deleteMyCv = %d %s", rec.Code, rec.Body)
	}
	if rec = e.callAs(t, app, e.unverified(), http.MethodGet, "/api/v1/me/cvs"); rec.Code != http.StatusForbidden ||
		code(rec) != "students.verification_required" {
		t.Fatalf("non-student = %d %s", rec.Code, rec.Body)
	}
}
