//go:build integration

package migrations_test

import (
	"maps"
	"slices"
	"strings"
	"uuid"

	"koro/internal/platform/db"
)

// Test helpers of the P1 schema-2 tests (migrations 000044–000064: students,
// entitlements, events). They extend p1aWorld: every row is built through
// the app_rw pool, so a missing GRANT shows up as a failing fixture.

// insertSQL builds "INSERT INTO table (cols) VALUES (exprs)" from the
// defaults overridden by over. Both map a column to a SQL expression
// ($n parameters allowed); an override of "-" drops the column, so a test can
// leave out a NOT NULL column. Columns are sorted: the text is stable.
func insertSQL(table string, defaults, over map[string]string) string {
	merged := maps.Clone(defaults)
	maps.Copy(merged, over)
	for c, v := range merged {
		if v == "-" {
			delete(merged, c)
		}
	}
	cols := slices.Sorted(maps.Keys(merged))
	vals := make([]string, len(cols))
	for i, c := range cols {
		vals[i] = merged[c]
	}
	return "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(vals, ", ") + ")"
}

// cols merges column overrides; later maps win.
func cols(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// lit returns s as a SQL string literal (the tests never pass a quote).
func lit(s string) string { return "'" + s + "'" }

// student inserts an active adult student user and returns its id.
func (w *p1aWorld) student() uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO users (id, status, email, email_normalized, first_name, last_name, birth_date, signup_persona)
		VALUES ($1, 'active', $2, $2, 'Ece', 'Şahin', DATE '2004-05-17', 'student')`, id, "s"+w.n()+"@example.test")
	return id
}

// profile inserts the student_profiles row of user at the university with
// the given targeting keys.
func (w *p1aWorld) profile(user, university uuid.UUID, eligible bool, keys ...string) {
	w.t.Helper()
	w.exec(`INSERT INTO student_profiles (user_id, university_id, degree_level, audience_eligible, audience_keys)
		VALUES ($1, $2, 'bachelor', $3, $4::text[])`, user, university, eligible, db.TextArray(keys))
}

// eventDefaults are the columns of a valid published in-person event;
// $1 = org, $2 = creator, $3 = event id.
var eventDefaults = map[string]string{
	"id":           "$3",
	"org_id":       "$1",
	"created_by":   "$2",
	"title":        "'Yazılım Atölyesi'",
	"slug":         "'e-' || gen_random_uuid()::text",
	"category":     "'workshop'",
	"format":       "'in_person'",
	"starts_at":    "now() + interval '1 day'",
	"ends_at":      "now() + interval '2 days'",
	"status":       "'published'",
	"published_at": "now()",
}

// event inserts a published event of the org (columns replaced by over) and
// returns its id.
func (w *p1aWorld) event(org, creator uuid.UUID, over map[string]string) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(insertSQL("events", eventDefaults, over), org, creator, id)
	return id
}

// registrationDefaults are the columns of a confirmed registration;
// $1 = event, $2 = org, $3 = user, $4 = registration id.
var registrationDefaults = map[string]string{
	"id":               "$4",
	"event_id":         "$1",
	"org_id":           "$2",
	"user_id":          "$3",
	"status":           "'confirmed'",
	"confirmed_at":     "now()",
	"source":           "'web'",
	"answers_purge_at": "now() + interval '100 days'",
}

// registration inserts a confirmed registration (columns replaced by over)
// and returns its id.
func (w *p1aWorld) registration(event, org, user uuid.UUID, over map[string]string) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(insertSQL("event_registrations", registrationDefaults, over), event, org, user, id)
	return id
}

// ticketDefaults are the columns of an active ticket; $1 = registration,
// $2 = event, $3 = ticket id, $4 = token digest, $5 = manual code.
var ticketDefaults = map[string]string{
	"id":              "$3",
	"registration_id": "$1",
	"event_id":        "$2",
	"version":         "1",
	"key_id":          "'k1'",
	"token_lookup":    "$4",
	"manual_code":     "$5",
}

// manualCode returns a valid Crockford manual code that is new in this world.
func (w *p1aWorld) manualCode() string {
	n := w.n()
	return strings.Repeat("0", 8-len(n)) + n
}

// ticket inserts an active ticket (columns replaced by over) and returns its id.
func (w *p1aWorld) ticket(registration, event uuid.UUID, over map[string]string) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(insertSQL("registration_tickets", ticketDefaults, over), registration, event, id, bytes32("ticket-"+w.n()), w.manualCode())
	return id
}

// checkinDefaults are the columns of a valid check-in; $1 = event, $2 = org,
// $3 = registration, $4 = ticket, $5 = staff, $6 = check-in id.
var checkinDefaults = map[string]string{
	"id":              "$6",
	"event_id":        "$1",
	"org_id":          "$2",
	"registration_id": "$3",
	"ticket_id":       "$4",
	"staff_user_id":   "$5",
	"method":          "'qr'",
	"client_scan_id":  "gen_random_uuid()",
	"scanned_at":      "now()",
}

// checkinArgs orders the parameters of checkinDefaults.
func checkinArgs(event, org, registration, ticket, staff, id uuid.UUID) []any {
	return []any{event, org, registration, ticket, staff, id}
}

// form inserts the event_forms row of the event and returns its id.
func (w *p1aWorld) form(event, org uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO event_forms (id, event_id, org_id) VALUES ($1, $2, $3)`, id, event, org)
	return id
}

// formVersionDefaults are the columns of a clean published version;
// $1 = form, $2 = event, $3 = publisher, $4 = version id, $5 = digest.
var formVersionDefaults = map[string]string{
	"id":                   "$4",
	"form_id":              "$1",
	"event_id":             "$2",
	"version_no":           "1",
	"schema":               `'{"schema_version":1,"fields":[]}'::jsonb`,
	"schema_sha256":        "$5",
	"field_count":          "0",
	"has_sensitive_fields": "false",
	"lint_report":          `'{"blocked":[],"warn_special":[],"review_identifier":[]}'::jsonb`,
	"published_by":         "$3",
}

// formVersion inserts a version of the form (columns replaced by over) and
// returns its id.
func (w *p1aWorld) formVersion(form, event, publisher uuid.UUID, over map[string]string) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(insertSQL("event_form_versions", formVersionDefaults, over), form, event, publisher, id, bytes32("form-"+w.n()))
	return id
}

// entitlementDefaults are the columns of an active order-line quota of ten
// targeted posts; $1 = org, $2 = source id.
var entitlementDefaults = map[string]string{
	"org_id":         "$1",
	"feature":        "'targeted_post'",
	"kind":           "'quota'",
	"quantity_total": "10",
	"valid_from":     "now()",
	"valid_to":       "now() + interval '30 days'",
	"status":         "'active'",
	"source_type":    "'order_line'",
	"source_id":      "$2",
	"activated_at":   "now()",
}

// entitlement inserts an entitlement (columns replaced by over) and returns
// its id.
func (w *p1aWorld) entitlement(org uuid.UUID, over map[string]string) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	defaults := maps.Clone(entitlementDefaults)
	defaults["id"] = "$3"
	w.exec(insertSQL("entitlements", defaults, over), org, uuid.NewV7(), id)
	return id
}
