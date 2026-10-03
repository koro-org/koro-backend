//go:build integration

package migrations_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// retentionSeedCodes are the 44 rows of 02 §24.2, in table order, and the
// one row 000121 adds (BE-049).
var retentionSeedCodes = []string{
	"outbox_published", "webhook_payload", "webhook_row", "idempotency", "audit_events", "audit_roots", "files_orphan",
	"edu_document", "event_export_file", "dsr_export_file", "consent_proof", "session_expired", "email_token",
	"edu_verification_history", "role_history", "notification_delivery", "email_suppression", "push_token",
	"in_app_notification", "membership_ended", "membership_ended_sensitive", "org_invitation", "club_claim",
	"access_grant_revoked", "event_form_answers", "event_form_answers_sensitive", "event_staff", "event_export_log",
	"post_removed", "content_report", "moderation_action", "outreach_dispatch", "contact_request_closed",
	"talent_ledger", "talent_counters", "talent_runs", "talent_neutralization", "entitlement_other",
	"financial_records", "dsr_record", "destruction_log", "breach_record", "dsr_erasure", "dormant_account",
	"privileged_action_history", // 000121 (BE-049): privileged_account_actions of BE-016, with role_history
}

// TestSchema_RetentionSeedComplete (02 §24.2, T-35): exactly the 44 policies
// of the table, none legally approved yet, each with the period, anchor and
// method of the table; only dormant_account is disabled. Re-running the
// seed never overwrites an approved or edited row.
func TestSchema_RetentionSeedComplete(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	if len(retentionSeedCodes) != 45 {
		t.Fatalf("the §24.2 list has %d codes, want 45", len(retentionSeedCodes))
	}
	got := queryStrings(t, w, `SELECT code FROM retention_policies ORDER BY code`)
	want := slices.Sorted(slices.Values(retentionSeedCodes))
	if !slices.Equal(got, want) {
		t.Fatalf("retention policies:\n got %v\nwant %v", got, want)
	}
	if n := w.count(`SELECT count(*) FROM retention_policies WHERE legal_approved OR approved_ref IS NOT NULL OR updated_by IS NOT NULL`); n != 0 {
		t.Errorf("%d seeded policies are marked as legally approved; all must start unapproved (T-35)", n)
	}
	if got := queryStrings(t, w, `SELECT code FROM retention_policies WHERE NOT enabled ORDER BY code`); !slices.Equal(got, []string{"dormant_account"}) {
		t.Errorf("disabled policies = %v, want only dormant_account", got)
	}
	// Spot checks of the table: period, anchor, method, target and owner.
	for code, row := range map[string]string{
		"outbox_published":             "7 days published_at hard_delete outbox platform",
		"webhook_payload":              "90 days created_at null_columns webhook_inbox platform",
		"idempotency":                  "24:00:00 expires_at hard_delete idempotency_keys platform",
		"audit_events":                 "2 years created_at hard_delete audit_events audit",
		"audit_roots":                  "10 years created_at hard_delete audit_daily_roots audit",
		"files_orphan":                 "24:00:00 created_at file_delete files:* files",
		"consent_proof":                "3 years withdrawn_or_erased hard_delete consents, iys_submissions consent",
		"push_token":                   "90 days last_seen_at hard_delete push_tokens notify",
		"membership_ended_sensitive":   "00:00:00 ended_at hard_delete org_members orgs",
		"club_claim":                   "1 year decided_at hard_delete club_claims directory",
		"event_form_answers_sensitive": "30 days event_end null_columns event_registrations.answers_sensitive_enc events",
		"post_removed":                 "1 year closed_at hard_delete posts feed",
		"moderation_action":            "2 years created_at hard_delete moderation_actions feed",
		"contact_request_closed":       "1 year closed_at hard_delete contact_requests messaging",
		"talent_ledger":                "2 years created_at hard_delete talent.point_events talent",
		"talent_counters":              "14 days created_at hard_delete talent.daily_counters talent",
		"talent_neutralization":        "3 years ended_at hard_delete talent.neutralizations talent",
		"financial_records":            "10 years created_at hard_delete",
		"dsr_record":                   "3 years closed_at hard_delete data_subject_requests kvkk",
		"breach_record":                "5 years closed_at hard_delete breach_incidents kvkk",
		"dsr_erasure":                  "00:00:00 created_at hard_delete users kvkk",
		"dormant_account":              "100 years last_seen_at anonymize users identity",
	} {
		got := queryString(t, w.d, `SELECT retention::text || ' ' || anchor || ' ' || method || ' ' || target || ' ' || owner_module FROM retention_policies WHERE code = $1`, code)
		if code == "financial_records" {
			got = queryString(t, w.d, `SELECT retention::text || ' ' || anchor || ' ' || method FROM retention_policies WHERE code = $1`, code)
		}
		if got != row {
			t.Errorf("%s = %q, want %q", code, got, row)
		}
	}
	if n := w.count(`SELECT count(*) FROM retention_policies WHERE legal_basis = '' OR description_tr = ''`); n != 0 {
		t.Errorf("%d policies without a legal basis or a description", n)
	}

	// Re-running the seed keeps the edited row and brings back a deleted one.
	owner := testutil.OwnerConn(t, w.d)
	w.exec(`UPDATE retention_policies SET retention = interval '60 days', legal_approved = true, approved_ref = 'Hukuk görüşü 2026/14' WHERE code = 'push_token'`)
	if _, err := owner.Exec(t.Context(), `DELETE FROM retention_policies WHERE code = 'webhook_row'`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000090_seed_retention_policies.up.sql")); err != nil {
		t.Fatalf("re-running the seed: %v", err)
	}
	if got := queryString(t, w.d, `SELECT retention::text || ' ' || legal_approved::text FROM retention_policies WHERE code = 'push_token'`); got != "60 days true" {
		t.Errorf("the approved policy was overwritten: %s", got)
	}
	if n := w.count(`SELECT count(*) FROM retention_policies`); n != 45 {
		t.Errorf("policies after the re-run = %d, want 45", n)
	}
}

// TestSchema_RetentionPolicyConstraints (02 §16.2 + the two additions).
func TestSchema_RetentionPolicyConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	ins := `INSERT INTO retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, legal_approved, approved_ref, description_tr)
		VALUES ($1, $2, 'x', $3::interval, $4, $5, 'öneri', $6, $7, 'Açıklama')`
	chk := db.CodeCheckViolation
	expectFail(t, w.d.Pool, []p1aCase{
		{"a code with a capital", ins, []any{"Bad_code", "kvkk", "1 day", "created_at", "hard_delete", false, nil}, chk, "retention_policies_code_chk"},
		{"a module with a digit", ins, []any{"new_policy", "kvkk2", "1 day", "created_at", "hard_delete", false, nil}, chk, "retention_policies_owner_module_chk"},
		{"a negative period", ins, []any{"new_policy", "kvkk", "-1 day", "created_at", "hard_delete", false, nil}, chk, "retention_policies_retention_chk"},
		{"unknown anchor", ins, []any{"new_policy", "kvkk", "1 day", "deleted_at", "hard_delete", false, nil}, chk, "retention_policies_anchor_chk"},
		{"unknown method", ins, []any{"new_policy", "kvkk", "1 day", "created_at", "shred", false, nil}, chk, "retention_policies_method_chk"},
		{"approved without a reference", ins, []any{"new_policy", "kvkk", "1 day", "created_at", "hard_delete", true, nil}, chk, "retention_policies_approval_chk"},
		{"an existing code", ins, []any{"push_token", "notify", "1 day", "created_at", "hard_delete", false, nil}, db.CodeUniqueViolation, "retention_policies_pkey"},
		{"approving a seeded policy without a reference", `UPDATE retention_policies SET legal_approved = true WHERE code = 'outbox_published'`, nil, chk, "retention_policies_approval_chk"},
		{"app_rw cannot delete a policy", `DELETE FROM retention_policies WHERE code = 'outbox_published'`, nil, db.CodeInsufficientPriv, ""},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"a policy of zero (delete at once)", ins, []any{"immediate_policy", "kvkk", "0", "ended_at", "hard_delete", false, nil}, "", ""},
		{"an approved policy with its reference", ins, []any{"approved_policy", "kvkk", "2 years", "created_at", "anonymize", true, "Toplantı 2026-10"}, "", ""},
		{"disabling a policy", `UPDATE retention_policies SET enabled = false WHERE code = 'immediate_policy'`, nil, "", ""},
	})
}

// TestSchema_DestructionLogsAppendOnly (02 §16.3, §0.5): the KVKK
// destruction record takes inserts only: app_rw has no UPDATE/DELETE/
// TRUNCATE grant and koro_forbid_mutation stops even the owner; a row names
// an existing policy.
func TestSchema_DestructionLogsAppendOnly(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	run := uuid.NewV7()
	ins := `INSERT INTO destruction_logs (id, policy_code, run_id, module, target, method, record_count, period_cutoff, started_at, finished_at, details)
		VALUES ($1, $2, $3, 'notify', 'notification_deliveries', $4, $5, now() - interval '12 months', now() - interval '1 minute', now(), $6)`
	first, second := uuid.NewV7(), uuid.NewV7()
	w.exec(ins, first, "notification_delivery", run, "hard_delete", 1250, `{"batches":2}`)
	w.exec(ins, second, "dsr_erasure", run, "hard_delete", 3, `{}`) // an account erasure, one row per module

	// Layer 1: no grant for app_rw.
	expectFail(t, w.d.Pool, []p1aCase{
		{"app_rw update", `UPDATE destruction_logs SET record_count = 0 WHERE id = $1`, []any{first}, db.CodeInsufficientPriv, ""},
		{"app_rw delete", `DELETE FROM destruction_logs WHERE id = $1`, []any{first}, db.CodeInsufficientPriv, ""},
		{"app_rw truncate", `TRUNCATE destruction_logs`, nil, db.CodeInsufficientPriv, ""},
	})
	// Layer 2: the triggers hold against the owner.
	owner := testutil.OwnerConn(t, w.d)
	expectFail(t, owner, []p1aCase{
		{"owner update", `UPDATE destruction_logs SET record_count = 0 WHERE id = $1`, []any{first}, db.CodeRestrictViolation, ""},
		{"owner update to the same value", `UPDATE destruction_logs SET details = details WHERE id = $1`, []any{second}, db.CodeRestrictViolation, ""},
		{"owner delete", `DELETE FROM destruction_logs WHERE id = $1`, []any{second}, db.CodeRestrictViolation, ""},
		{"owner truncate", `TRUNCATE destruction_logs`, nil, db.CodeRestrictViolation, ""},
		{"owner truncate cascade", `TRUNCATE destruction_logs CASCADE`, nil, db.CodeRestrictViolation, ""},
		// The record keeps its policy: the policy row cannot go.
		{"owner deletes a logged policy", `DELETE FROM retention_policies WHERE code = 'notification_delivery'`, nil, db.CodeForeignKeyViolation, "destruction_logs_policy_code_fkey"},
	})
	if n := w.count(`SELECT count(*) FROM destruction_logs WHERE run_id = $1`, run); n != 2 {
		t.Fatalf("destruction rows = %d, want 2: the record changed", n)
	}
	if got := queryStrings(t, w, `SELECT tgname FROM pg_trigger WHERE tgrelid = 'destruction_logs'::regclass AND NOT tgisinternal ORDER BY tgname`); !slices.Equal(got, []string{"destruction_logs_immutable", "destruction_logs_no_truncate"}) {
		t.Errorf("triggers on destruction_logs = %v", got)
	}
	expectFail(t, w.d.Pool, []p1aCase{
		{"unknown policy", ins, []any{uuid.NewV7(), "no_such_policy", run, "hard_delete", 1, `{}`}, db.CodeForeignKeyViolation, "destruction_logs_policy_code_fkey"},
		{"unknown method", ins, []any{uuid.NewV7(), "notification_delivery", run, "burn", 1, `{}`}, db.CodeCheckViolation, "destruction_logs_method_chk"},
		{"negative count", ins, []any{uuid.NewV7(), "notification_delivery", run, "hard_delete", -1, `{}`}, db.CodeCheckViolation, "destruction_logs_record_count_chk"},
	})
}

// TestSchema_DataSubjectRequestConstraints (02 §16.1): who asks, the
// statuses, the two approvers of a profiling access answer (the second one
// needs the first and is another person: the NULL gap of the 02 CHECK is
// closed).
func TestSchema_DataSubjectRequestConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	user, dpo, dpo2 := w.user(), w.user(), w.user()
	d := func(name string, over map[string]string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("data_subject_requests", dsrDefaults, over), args, state, constraint}
	}
	u := []any{user}
	chk := func(name string, over map[string]string, constraint string) p1aCase {
		return d(name, over, u, db.CodeCheckViolation, constraint)
	}
	first, second := lit(dpo.String()), lit(dpo2.String())
	expectFail(t, w.d.Pool, []p1aCase{
		chk("unknown type", map[string]string{"type": "'delete_everything'"}, "data_subject_requests_type_chk"),
		chk("unknown channel", map[string]string{"channel": "'fax'"}, "data_subject_requests_channel_chk"),
		chk("unknown status", map[string]string{"status": "'done'"}, "data_subject_requests_status_chk"),
		d("nobody to answer", map[string]string{"user_id": "NULL"}, u[:0], db.CodeCheckViolation, "data_subject_requests_requester_chk"),
		chk("the same approver twice", map[string]string{"first_approver": first, "second_approver": first}, "data_subject_requests_approvers_chk"),
		chk("a second approver without the first", map[string]string{"second_approver": second}, "data_subject_requests_approvers_chk"),
		chk("profiling access completed by one approver", map[string]string{"type": "'profiling_access'", "status": "'completed'", "completed_at": "now()", "first_approver": first}, "data_subject_requests_profiling_approval_chk"),
		chk("completed without a completion time", map[string]string{"status": "'completed'"}, "data_subject_requests_completed_chk"),
		chk("rejected without a completion time", map[string]string{"status": "'rejected'", "rejection_reason": "'Kimlik doğrulanamadı'"}, "data_subject_requests_completed_chk"),
		chk("a completion time on an open request", map[string]string{"completed_at": "now()"}, "data_subject_requests_completed_chk"),
		chk("rejected without a reason", map[string]string{"status": "'rejected'", "completed_at": "now()"}, "data_subject_requests_rejected_chk"),
		chk("details of 4001 characters", map[string]string{"details": lit(repeat("a", 4001))}, "data_subject_requests_details_chk"),
		chk("an e-mail of 255 characters", map[string]string{"requester_email": lit(repeat("a", 255))}, "data_subject_requests_requester_email_chk"),
		d("no due date", map[string]string{"due_at": "-"}, u, codeNotNull, ""),
		d("unknown user", nil, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "data_subject_requests_user_id_fkey"),
		d("unknown export file", map[string]string{"export_file_id": lit(uuid.NewV7().String())}, u, db.CodeForeignKeyViolation, "data_subject_requests_export_file_id_fkey"),
	})
	expectOK(t, w.d.Pool, []p1aCase{
		d("an in-app access request", nil, u, "", ""),
		d("an unregistered applicant by e-mail", map[string]string{"user_id": "NULL", "requester_name": "'Ayşe Kaya'", "requester_email": "'ayse@example.test'", "channel": "'email'"}, u[:0], "", ""),
		d("profiling access with two approvers", map[string]string{"type": "'profiling_access'", "status": "'completed'", "completed_at": "now()", "first_approver": first, "second_approver": second}, u, "", ""),
		d("awaiting the second approval", map[string]string{"type": "'profiling_access'", "status": "'awaiting_second_approval'", "first_approver": first}, u, "", ""),
		d("rejected with a reason", map[string]string{"status": "'rejected'", "completed_at": "now()", "rejection_reason": "'Kimlik doğrulanamadı'"}, u, "", ""),
		d("cancelled", map[string]string{"status": "'cancelled'"}, u, "", ""),
		d("an erasure request", map[string]string{"type": "'erasure'", "identity_verified_at": "now()"}, u, "", ""),
	})
}

// TestSchema_BreachAndSubprocessorConstraints (02 §16.4, §16.5).
func TestSchema_BreachAndSubprocessorConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	owner := w.user()
	breach := `INSERT INTO breach_incidents (title, detected_at, authority_deadline_at, description, status, owner_user_id, closed_at, affected_estimate)
		VALUES ($1, now(), now() + interval '72 hours', 'Yanlış alıcıya e-posta', $2, $3, $4, $5)`
	sub := `INSERT INTO subprocessors (name, service, data_categories, region, country_code, transfer_mechanism) VALUES ($1, 'E-posta', ARRAY['iletişim'], $2, $3, $4)`
	expectFail(t, w.d.Pool, []p1aCase{
		{"closed without a closing time", breach, []any{"İhlal", "closed", owner, nil, nil}, db.CodeCheckViolation, "breach_incidents_closed_chk"},
		{"a closing time on an open incident", breach, []any{"İhlal", "open", owner, "2026-10-03T00:00:00Z", nil}, db.CodeCheckViolation, "breach_incidents_closed_chk"},
		{"unknown status", breach, []any{"İhlal", "solved", owner, nil, nil}, db.CodeCheckViolation, "breach_incidents_status_chk"},
		{"a title of 201 characters", breach, []any{repeat("a", 201), "open", owner, nil, nil}, db.CodeCheckViolation, "breach_incidents_title_chk"},
		{"a negative estimate", breach, []any{"İhlal", "open", owner, nil, -1}, db.CodeCheckViolation, "breach_incidents_affected_estimate_chk"},
		{"unknown owner", breach, []any{"İhlal", "open", uuid.NewV7(), nil, nil}, db.CodeForeignKeyViolation, "breach_incidents_owner_user_id_fkey"},
		{"a foreign processor called domestic", sub, []any{"Mailer Inc", "eu-west-1", "IE", "domestic"}, db.CodeCheckViolation, "subprocessors_domestic_chk"},
		{"a lower-case country", sub, []any{"Mailer Inc", "eu-west-1", "ie", "standard_contract"}, db.CodeCheckViolation, "subprocessors_country_code_chk"},
		{"unknown transfer mechanism", sub, []any{"Mailer Inc", "eu-west-1", "IE", "hope"}, db.CodeCheckViolation, "subprocessors_transfer_mechanism_chk"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"an open incident", breach, []any{"E-posta ihlali", "open", owner, nil, 12}, "", ""},
		{"a closed incident", breach, []any{"Eski ihlal", "closed", owner, "2026-10-03T00:00:00Z", nil}, "", ""},
		{"a domestic processor", sub, []any{"Yerel Bulut", "İstanbul", "TR", "domestic"}, "", ""},
		{"a foreign processor under a standard contract", sub, []any{"Mailer Inc", "eu-west-1", "IE", "standard_contract"}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"the same processor twice", sub, []any{"Mailer Inc", "us-east-1", "US", "undecided"}, db.CodeUniqueViolation, "subprocessors_name_uq"},
		{"app_rw cannot delete a processor", `DELETE FROM subprocessors WHERE name = 'Mailer Inc'`, nil, db.CodeInsufficientPriv, ""},
	})
	w.exec(`UPDATE subprocessors SET active = false WHERE name = 'Mailer Inc'`)
}
