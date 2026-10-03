package migrations_test

import (
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"

	"koro/db/migrations"
)

// p1aFiles is the file order of 000010–000043 in 02 §23 (BE-009, BE-001 R2).
// The names are a contract: later tickets, the design documents and the
// down/up tests refer to them.
var p1aFiles = []string{
	"000010_create_consent_purposes",
	"000011_seed_consent_purposes",
	"000012_create_notice_versions",
	"000013_create_consents",
	"000014_create_view_consent_current",
	"000015_create_iys_submissions",
	"000016_create_users",
	"000017_add_fk_consents_user",
	"000018_add_fk_notice_versions_created_by",
	"000019_create_sessions",
	"000020_create_email_tokens",
	"000021_create_edu_verifications",
	"000022_create_mfa_totp",
	"000023_create_mfa_recovery_codes",
	"000024_create_platform_role_assignments",
	"000025_create_notification_deliveries",
	"000026_create_email_suppressions",
	"000027_create_push_tokens",
	"000028_create_in_app_notifications",
	"000029_create_notification_preferences",
	"000030_create_organizations",
	"000031_seed_platform_org",
	"000032_add_fk_in_app_notifications_source_org",
	"000033_create_companies",
	"000034_create_org_members",
	"000035_create_org_invitations",
	"000036_create_universities",
	"000037_add_fk_edu_verifications_university",
	"000038_create_university_email_domains",
	"000039_create_faculties",
	"000040_create_departments",
	"000041_create_clubs",
	"000042_create_club_claims",
	"000043_create_skills",
}

// TestMigrations_P1Schema1FileNames: numbers 000010–000043 exist exactly with
// the names of 02 §23, each as an up/down pair, and every up file states the
// lock timeout of its transaction (05 §7.2).
func TestMigrations_P1Schema1FileNames(t *testing.T) {
	t.Parallel()
	var got []string
	for n := 10; n <= 43; n++ {
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
	if !slices.Equal(got, p1aFiles) {
		t.Errorf("migration names 000010–000043 differ from 02 §23:\n got %v\nwant %v", got, p1aFiles)
	}
}
