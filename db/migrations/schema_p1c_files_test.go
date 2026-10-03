package migrations_test

import (
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"koro/db/migrations"
)

// p1cFiles is the file order of 000065–000093 in 02 §23 (BE-011, BE-001 R2).
// Like p1aFiles and p1bFiles, the names are a contract.
var p1cFiles = []string{
	"000065_create_feed_org_settings",
	"000066_create_posts",
	"000067_create_post_media",
	"000068_create_post_reactions",
	"000069_create_post_saves",
	"000070_create_post_hides",
	"000071_create_org_follows",
	"000072_create_org_mutes",
	"000073_create_content_reports",
	"000074_create_moderation_actions",
	"000075_create_segments",
	"000076_create_outreach_dispatches",
	"000077_create_contact_requests",
	"000078_create_contact_blocks",
	"000079_create_talent_config_versions",
	"000080_create_talent_signal_defs",
	"000081_seed_talent_signal_defs",
	"000082_create_talent_point_events",
	"000083_create_talent_daily_counters",
	"000084_create_talent_cohorts",
	"000085_create_talent_scores",
	"000086_create_talent_score_runs",
	"000087_create_talent_neutralizations",
	"000088_create_data_subject_requests",
	"000089_create_retention_policies",
	"000090_seed_retention_policies",
	"000091_create_destruction_logs",
	"000092_create_breach_incidents",
	"000093_create_subprocessors",
}

// TestMigrations_P1Schema3FileNames: numbers 000065–000093 exist exactly with
// the names of 02 §23, each as an up/down pair starting with its name
// comment, and every up file states the lock timeout of its transaction
// (05 §7.2).
func TestMigrations_P1Schema3FileNames(t *testing.T) {
	t.Parallel()
	var got []string
	for n := 65; n <= 93; n++ {
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
			if !strings.HasPrefix(string(src), "-- "+strings.TrimSuffix(file, ".sql")) {
				t.Errorf("%s: the first line must be the comment \"-- %s…\"", file, strings.TrimSuffix(file, ".sql"))
			}
		}
		up, _ := fs.ReadFile(migrations.FS, ups[0])
		if !strings.Contains(string(up), "SET LOCAL lock_timeout = '5s';") {
			t.Errorf("%s: missing SET LOCAL lock_timeout = '5s'", ups[0])
		}
	}
	if !slices.Equal(got, p1cFiles) {
		t.Errorf("migration names 000065–000093 differ from 02 §23:\n got %v\nwant %v", got, p1cFiles)
	}
}

var (
	sqlLineComment = regexp.MustCompile(`(?m)--.*$`)
	sqlDollarBody  = regexp.MustCompile(`(?s)\$\$.*?\$\$`)
	sqlStringLit   = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlAndWord     = regexp.MustCompile(`(?i)\bAND\b`)
)

// stateConjunctions returns every "= (… AND …)" of src: an equality whose
// right side is a parenthesised conjunction (AND directly inside the
// parentheses). Comments, string literals and function bodies are ignored.
func stateConjunctions(src string) []string {
	s := sqlLineComment.ReplaceAllString(src, "")
	s = sqlDollarBody.ReplaceAllString(s, "$$$$")
	s = sqlStringLit.ReplaceAllString(s, "''")
	var out []string
	for i := range len(s) {
		if s[i] != '=' || (i > 0 && strings.ContainsRune("<>!=", rune(s[i-1]))) {
			continue
		}
		j := i + 1
		for j < len(s) && (s[j] == ' ' || s[j] == '\n' || s[j] == '\t') {
			j++
		}
		if j >= len(s) || s[j] != '(' {
			continue
		}
		// Collect the text directly inside the group (depth 1 only).
		depth, end := 0, -1
		var direct strings.Builder
		for k := j; k < len(s); k++ {
			switch s[k] {
			case '(':
				depth++
				if depth > 1 {
					direct.WriteByte(' ')
				}
				continue
			case ')':
				depth--
				if depth == 0 {
					end = k
				}
			}
			if end >= 0 {
				break
			}
			if depth == 1 {
				direct.WriteByte(s[k])
			}
		}
		if end > 0 && sqlAndWord.MatchString(direct.String()) {
			from := max(0, i-60)
			out = append(out, strings.Join(strings.Fields(s[from:end+1]), " "))
		}
	}
	return out
}

// TestMigrations_NoStateEqualsConjunctionCheck (06 H-37, T-131): the pattern
// CHECK ((state) = (A IS NOT NULL AND B IS NOT NULL)) is forbidden, because
// a row that is not in the state passes with only A (or only B) set
// (false = false). Each column is tied to the state on its own, or the
// state is written as an implication per state. The check runs on every up
// file, so a later schema ticket cannot bring the pattern back.
func TestMigrations_NoStateEqualsConjunctionCheck(t *testing.T) {
	t.Parallel()
	ups, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil || len(ups) == 0 {
		t.Fatalf("no up files: %v", err)
	}
	for _, file := range ups {
		src, err := fs.ReadFile(migrations.FS, file)
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range stateConjunctions(string(src)) {
			t.Errorf("%s: forbidden \"(state) = (A AND B)\" CHECK (06 H-37): …%s", file, hit)
		}
	}
}

// TestMigrations_StateConjunctionDetector pins the detector of the H-37 test
// on the old (broken) and the fixed forms, so the guard cannot silently
// stop matching.
func TestMigrations_StateConjunctionDetector(t *testing.T) {
	t.Parallel()
	bad := []string{
		`CHECK ((status = 'revoked') = (revoked_at IS NOT NULL AND revoke_reason IS NOT NULL))`,
		`CHECK ((status IN ('approved','rejected')) = (reviewed_by IS NOT NULL AND reviewed_at IS NOT NULL))`,
		`CHECK ((status = 'active')  = (joined_at IS NOT NULL AND ended_at IS NULL))`,
		"CHECK ((method = 'edu_email') = (edu_email IS NOT NULL\n  and email_domain IS NOT NULL))",
		`CHECK (paid = ((paid_at IS NOT NULL) AND (paid_by IS NOT NULL)))`,
	}
	for _, src := range bad {
		if len(stateConjunctions(src)) != 1 {
			t.Errorf("not detected: %s", src)
		}
	}
	good := []string{
		`CHECK ((status = 'revoked') = (revoked_at IS NOT NULL) AND (status = 'revoked') = (revoke_reason IS NOT NULL))`,
		`CHECK (status <> 'active' OR (joined_at IS NOT NULL AND ended_at IS NULL))`,
		`CHECK (audience_kind = 'segment' OR (audience_keys = ARRAY['all']::text[] AND audience_dim_count = 1))`,
		`CHECK (has_sensitive_fields = (coalesce(lint_report -> 'warn_special', '[]'::jsonb) <> '[]'::jsonb))`,
		`CHECK (requires_registration OR (capacity IS NULL AND NOT waitlist_enabled))`,
		`-- old: (status = 'x') = (a IS NOT NULL AND b IS NOT NULL)`,
		`COMMENT ON TABLE x IS '(s) = (a AND b)'`,
		`CHECK (x >= (a AND b))`,
		"CREATE FUNCTION f() RETURNS trigger AS $$ BEGIN IF (a) = (b AND c) THEN RETURN NEW; END IF; END $$;",
	}
	for _, src := range good {
		if hits := stateConjunctions(src); len(hits) != 0 {
			t.Errorf("false positive %v: %s", hits, src)
		}
	}
}
