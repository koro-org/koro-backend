package authz_test

import (
	"testing"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// TestRecommendPolicy_OwnForVerifiedStudents pins 03 §5.24 (BE-174) for the
// 18 personas: the caller's own recommendations are O for the verified
// students (student, student_minor and the club and staff personas, who are
// verified students), 403 for every other signed-in user (usr: k-uv;
// companies and platform roles: no cell) and refused for anonymous (401
// over HTTP); another user's recommendations are 404; another resource
// type is never accepted.
func TestRecommendPolicy_OwnForVerifiedStudents(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	act := authz.RecommendRecommendationListOwn
	mine := func(p authztest.Persona) authz.Resource {
		u := p.User()
		return authz.Resource{Type: authz.TypeMyRecommendations, OwnerUserID: &u}
	}
	students := map[string]bool{
		"student": true, "student_minor": true, "club_member": true, "club_admin": true, "other_club_admin": true, "event_staff": true,
	}
	for _, p := range authztest.Personas() {
		var want error = authz.ErrForbidden
		if students[p.Name] {
			want = nil
		}
		t.Run("own/"+p.Name, func(t *testing.T) {
			t.Parallel()
			wantErr(t, authz.Can(ctx, p.Actor, act, mine(p)), want, false)
		})
		if p.Name == "anonymous" {
			continue
		}
		t.Run("other/"+p.Name, func(t *testing.T) {
			t.Parallel()
			other := authztest.ID("student")
			if p.Name == "student" {
				other = authztest.ID("student_minor")
			}
			wantErr(t, authz.Can(ctx, p.Actor, act, authz.Resource{Type: authz.TypeMyRecommendations, OwnerUserID: &other}),
				authz.ErrNotFound, false)
		})
	}
	st := persona(t, "student")
	u := st.UserID
	wantErr(t, authz.Can(ctx, st, act, authz.Resource{Type: authz.TypeStudentProfile, OwnerUserID: &u,
		Attrs: map[string]string{authz.AttrAccessGrant: "none", "discoverable": "false", "minor": "false"}}), authz.ErrNotFound, false)
}
