package migrations_test

import (
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"

	"koro/db/migrations"
)

// p1bFiles is the file order of 000044–000064 in 02 §23 (BE-010, BE-001 R2).
// Like p1aFiles, the names are a contract.
var p1bFiles = []string{
	"000044_create_student_profiles",
	"000045_create_function_count_segment",
	"000046_create_student_privacy_settings",
	"000047_create_student_skills",
	"000048_create_student_experiences",
	"000049_create_student_languages",
	"000050_create_student_cvs",
	"000051_create_profile_access_grants",
	"000052_create_entitlements",
	"000053_create_entitlement_usages",
	"000054_create_events",
	"000055_create_event_staff",
	"000056_create_event_forms",
	"000057_create_event_form_versions",
	"000058_add_fk_event_forms_current_version",
	"000059_create_form_lint_rules",
	"000060_seed_form_lint_rules",
	"000061_create_event_registrations",
	"000062_create_registration_tickets",
	"000063_create_event_checkins",
	"000064_create_event_data_exports",
}

// TestMigrations_P1Schema2FileNames: numbers 000044–000064 exist exactly with
// the names of 02 §23, each as an up/down pair, and every up file states the
// lock timeout of its transaction (05 §7.2).
func TestMigrations_P1Schema2FileNames(t *testing.T) {
	t.Parallel()
	var got []string
	for n := 44; n <= 64; n++ {
		prefix := strconv.Itoa(n)
		prefix = strings.Repeat("0", 6-len(prefix)) + prefix
		ups, err := fs.Glob(migrations.FS, prefix+"_*.up.sql")
		if err != nil {
			t.Fatal(err)
		}
		downs, err := fs.Glob(migrations.FS, prefix+"_*.down.sql")
		if err != nil {
			t.Fatal(err)
		}
		if len(ups) != 1 || len(downs) != 1 {
			t.Errorf("migration %s: %d up and %d down files, want one pair", prefix, len(ups), len(downs))
			continue
		}
		name := strings.TrimSuffix(ups[0], ".up.sql")
		if strings.TrimSuffix(downs[0], ".down.sql") != name {
			t.Errorf("migration %s: up is %q but down is %q", prefix, ups[0], downs[0])
		}
		got = append(got, name)
		for _, file := range []string{ups[0], downs[0]} {
			src, err := fs.ReadFile(migrations.FS, file)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(src), "-- "+file[:len(file)-len(".sql")]) {
				t.Errorf("%s: the first line must be the comment \"-- %s…\"", file, strings.TrimSuffix(file, ".sql"))
			}
		}
		up, _ := fs.ReadFile(migrations.FS, ups[0])
		if !strings.Contains(string(up), "SET LOCAL lock_timeout = '5s';") {
			t.Errorf("%s: missing SET LOCAL lock_timeout = '5s'", ups[0])
		}
	}
	if !slices.Equal(got, p1bFiles) {
		t.Errorf("migration names 000044–000064 differ from 02 §23:\n got %v\nwant %v", got, p1bFiles)
	}
}
