package students_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"koro/internal/students"
)

// TestHTTP_SelfSkills_Named (BE-123): putMySkills and getMyStudentProfile
// carry the catalog name of every skill (one SkillNamer call each), so the
// profile page needs no catalog lookup.
func TestHTTP_SelfSkills_Named(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	want := map[string]string{e.skill1.String(): "Go", e.skill2.String(): "SQL"}
	check := func(name string, items []students.StudentSkillDTO) {
		t.Helper()
		if len(items) != len(want) {
			t.Fatalf("%s: %d skills, want %d", name, len(items), len(want))
		}
		for _, k := range items {
			if k.Name != want[k.SkillID] {
				t.Fatalf("%s: skill %s named %q, want %q", name, k.SkillID, k.Name, want[k.SkillID])
			}
		}
	}

	e.skillN.Store(0)
	rec := call(t, app, "student", http.MethodPut, "/api/v1/me/student-profile/skills",
		`{"items":[{"skill_id":"`+e.skill1.String()+`","level":3},{"skill_id":"`+e.skill2.String()+`"}]}`, nil)
	var put students.StudentSkillList
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &put) != nil {
		t.Fatalf("putMySkills = %d %s", rec.Code, rec.Body)
	}
	check("putMySkills", put.Items)
	if n := e.skillN.Load(); n != 1 {
		t.Fatalf("SkillNames calls for putMySkills = %d, want 1", n)
	}

	e.skillN.Store(0)
	rec = call(t, app, "student", http.MethodGet, "/api/v1/me/student-profile", "", nil)
	var got students.StudentProfileSelfDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("getMyStudentProfile = %d %s", rec.Code, rec.Body)
	}
	check("getMyStudentProfile", got.Skills)
	if n := e.skillN.Load(); n != 1 {
		t.Fatalf("SkillNames calls for getMyStudentProfile = %d, want 1", n)
	}
}
