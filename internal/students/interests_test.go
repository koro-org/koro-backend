package students_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"koro/internal/authz"
	"koro/internal/students"
)

// TestUpdateProfile_Interests (BE-204, Q-57): the student replaces the
// interests through updateMyStudentProfile (a minor too: the student's own
// data); they are kept without duplicates in the list's order, audited as
// a changed field, read back by the profile and by the recommendations'
// read, and an empty list clears them. An unknown or sensitive value and
// more than MaxInterests are 422 before anything is written.
func TestUpdateProfile_Interests(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	if got, err := e.recommendations(t).Interests(ctx, a.UserID); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("Interests without a profile = %v, %v", got, err)
	}
	in := []students.Interest{students.InterestSports, students.InterestTechnology, students.InterestSports}
	v, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Interests: &in})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(v.Profile.Interests, []string{"technology", "sports"}) {
		t.Fatalf("stored interests = %v, want technology, sports (deduplicated, in order)", v.Profile.Interests)
	}
	rows := e.audit.Of(authz.StudentsProfileUpdateOwn)
	if len(rows) != 1 || !slices.Contains(rows[0].Metadata["fields"].([]string), "interests") {
		t.Fatalf("audit = %+v", rows)
	}
	got, err := e.recommendations(t).Interests(ctx, a.UserID)
	if err != nil || !slices.Equal(got, []students.Interest{students.InterestTechnology, students.InterestSports}) {
		t.Fatalf("Interests = %v, %v", got, err)
	}
	if v, err := e.svc.GetMyProfile(ctx, a); err != nil || !slices.Equal(v.Profile.Interests, []string{"technology", "sports"}) {
		t.Fatalf("GetMyProfile interests = %v, %v", v.Profile.Interests, err)
	}
	// The same set again changes nothing (no audit row).
	same := []students.Interest{students.InterestSports, students.InterestTechnology}
	if _, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Interests: &same}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.audit.Of(authz.StudentsProfileUpdateOwn)); n != 1 {
		t.Fatalf("an unchanged set wrote %d audit rows", n)
	}
	for name, bad := range map[string][]students.Interest{
		"sensitive": {students.InterestArts, "religious"},
		"unknown":   {"cooking"},
		"too many": {
			students.InterestAcademic, students.InterestTechnology, students.InterestCareer, students.InterestArts,
			students.InterestSports, students.InterestHobby,
		},
	} {
		_, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Interests: &bad, Headline: ptr("Değişmemeli")})
		wantCode(t, err, "validation_failed")
		if p := e.profile(t, a.UserID); !slices.Equal(p.Interests, []string{"technology", "sports"}) || p.Headline != nil {
			t.Fatalf("%s: a refused patch wrote %v / %v", name, p.Interests, p.Headline)
		}
	}
	empty := []students.Interest{}
	if v, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Interests: &empty}); err != nil || len(v.Profile.Interests) != 0 {
		t.Fatalf("clear = %v, %v", v.Profile.Interests, err)
	}
	m := e.student(t, e.uniA, true)
	mine := []students.Interest{students.InterestHobby}
	if v, err := e.svc.UpdateMyProfile(ctx, m, students.ProfilePatch{Interests: &mine}); err != nil ||
		!slices.Equal(v.Profile.Interests, []string{"hobby"}) {
		t.Fatalf("minor = %v, %v", v.Profile.Interests, err)
	}
}

// TestPrivacyProvider_ExportsInterests (BE-204, KVKK md. 11): the
// interests are in the student's export (the student_profile record) and
// go with the profile.
func TestPrivacyProvider_ExportsInterests(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	in := []students.Interest{students.InterestCareer}
	if _, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Interests: &in}); err != nil {
		t.Fatal(err)
	}
	sec, err := e.mod.Privacy.Export(ctx, a.UserID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range sec.Items {
		if it["record"] == "student_profile" {
			found = slices.Equal(it["interests"].([]string), []string{"career"})
		}
	}
	if !found {
		t.Fatalf("export = %+v, want the interests in student_profile", sec.Items)
	}
	if _, err := e.mod.Privacy.Erase(ctx, a.UserID); err != nil {
		t.Fatal(err)
	}
	if got, err := e.recommendations(t).Interests(ctx, a.UserID); err != nil || len(got) != 0 {
		t.Fatalf("Interests after erasure = %v, %v", got, err)
	}
}

// TestHTTP_UpdateProfile_Interests (BE-204): the contract refuses an
// unknown or sensitive value, a duplicate and more than five items (422);
// a valid list comes back in the student's own profile.
func TestHTTP_UpdateProfile_Interests(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	for name, body := range map[string]string{
		"sensitive": `{"interests":["religious"]}`,
		"unknown":   `{"interests":["cooking"]}`,
		"duplicate": `{"interests":["arts","arts"]}`,
		"too many":  `{"interests":["academic","technology","career","arts","sports","hobby"]}`,
	} {
		if rec := call(t, app, "student", http.MethodPatch, "/api/v1/me/student-profile", body, nil); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	rec := call(t, app, "student", http.MethodPatch, "/api/v1/me/student-profile",
		`{"interests":["social_responsibility","entrepreneurship"]}`, nil)
	var got students.StudentProfileSelfDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil ||
		!slices.Equal(got.Interests, []string{"entrepreneurship", "social_responsibility"}) {
		t.Fatalf("PATCH = %d %s", rec.Code, rec.Body)
	}
	rec = call(t, app, "student", http.MethodGet, "/api/v1/me/student-profile", "", nil)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || len(got.Interests) != 2 {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
}
