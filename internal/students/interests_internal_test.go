package students

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestInterests_EnumTagsMatchTheList (BE-204): the OpenAPI enum of the
// profile's interests (the student's own view and updateMyStudentProfile)
// is exactly Interests, in its order; no sensitive club category
// (religious, political, identity) and no "other" is one.
func TestInterests_EnumTagsMatchTheList(t *testing.T) {
	t.Parallel()
	want := make([]string, 0, len(interestOrder))
	for _, i := range Interests() {
		want = append(want, string(i))
	}
	self, _ := reflect.TypeFor[StudentProfileSelfDTO]().FieldByName("Interests")
	body, _ := reflect.TypeFor[updateProfileInput]().Field(0).Type.FieldByName("Interests")
	for name, tag := range map[string]reflect.StructTag{"self": self.Tag, "update": body.Tag} {
		if got := strings.Split(tag.Get("enum"), ","); !slices.Equal(got, want) {
			t.Fatalf("%s interests enum = %v, want %v", name, got, want)
		}
	}
	if body.Tag.Get("maxItems") != "5" || MaxInterests != 5 {
		t.Fatalf("maxItems = %q, MaxInterests = %d", body.Tag.Get("maxItems"), MaxInterests)
	}
	for _, bad := range []Interest{"religious", "political", "identity", "other", "health", ""} {
		if bad.Valid() {
			t.Fatalf("%q is an interest", bad)
		}
	}
}
