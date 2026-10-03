//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
)

// TestSchema_UserConstraints covers users (02 §5.1): the erased tombstone
// and live-account shapes, the student age gate input, the unique
// normalised e-mail (NULLs of tombstones do not collide) and the missing
// DELETE grant.
func TestSchema_UserConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	const live = `INSERT INTO users (status, email, email_normalized, first_name, last_name, signup_persona`
	expectOK(t, w.d.Pool, []p1aCase{
		{"live company user", live + `) VALUES ('active', 'a@example.test', 'a@example.test', 'Ada', 'Yılmaz', 'company')`, nil, "", ""},
		{"student with a birth date", live + `, birth_date) VALUES ('pending_email', 'b@example.test', 'b@example.test', 'Işık', 'Çelik', 'student', DATE '2005-05-17')`, nil, "", ""},
		{"first tombstone", `INSERT INTO users (status, signup_persona, erased_at) VALUES ('erased', 'student', now())`, nil, "", ""},
		{"second tombstone (NULL e-mails do not collide)", `INSERT INTO users (status, signup_persona, erased_at) VALUES ('erased', 'company', now())`, nil, "", ""},
		{"suspended with a time", live + `, suspended_at) VALUES ('suspended', 'c@example.test', 'c@example.test', 'Can', 'Demir', 'club', now())`, nil, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"duplicate normalised e-mail", live + `) VALUES ('active', 'A@Example.test', 'a@example.test', 'Ada', 'Yılmaz', 'company')`, nil, db.CodeUniqueViolation, "users_email_uq"},
		{"tombstone that kept the e-mail", `INSERT INTO users (status, email, email_normalized, signup_persona, erased_at) VALUES ('erased', 'x@example.test', 'x@example.test', 'company', now())`, nil, db.CodeCheckViolation, "users_erased_chk"},
		{"tombstone that kept the name", `INSERT INTO users (status, first_name, signup_persona, erased_at) VALUES ('erased', 'Ada', 'company', now())`, nil, db.CodeCheckViolation, "users_erased_chk"},
		{"tombstone that kept the password hash", `INSERT INTO users (status, password_hash, signup_persona, erased_at) VALUES ('erased', '$argon2id$x', 'company', now())`, nil, db.CodeCheckViolation, "users_erased_chk"},
		{"tombstone that kept the birth date", `INSERT INTO users (status, birth_date, signup_persona, erased_at) VALUES ('erased', DATE '2000-01-01', 'student', now())`, nil, db.CodeCheckViolation, "users_erased_chk"},
		{"tombstone without erased_at", `INSERT INTO users (status, signup_persona) VALUES ('erased', 'company')`, nil, db.CodeCheckViolation, "users_erased_chk"},
		{"live user without a name", `INSERT INTO users (status, email, email_normalized, signup_persona) VALUES ('active', 'n@example.test', 'n@example.test', 'company')`, nil, db.CodeCheckViolation, "users_live_chk"},
		{"live user without an e-mail", `INSERT INTO users (status, first_name, last_name, signup_persona) VALUES ('active', 'Ada', 'Yılmaz', 'company')`, nil, db.CodeCheckViolation, "users_live_chk"},
		{"suspended without suspended_at", live + `) VALUES ('suspended', 'd@example.test', 'd@example.test', 'Ada', 'Yılmaz', 'company')`, nil, db.CodeCheckViolation, "users_suspended_chk"},
		{"student without a birth date", live + `) VALUES ('active', 'e@example.test', 'e@example.test', 'Ada', 'Yılmaz', 'student')`, nil, db.CodeCheckViolation, "users_student_birth_date_chk"},
		{"birth date before 1900", live + `, birth_date) VALUES ('active', 'f@example.test', 'f@example.test', 'Ada', 'Yılmaz', 'student', DATE '1899-12-31')`, nil, db.CodeCheckViolation, "users_birth_date_chk"},
		{"unknown status", live + `) VALUES ('banned', 'g@example.test', 'g@example.test', 'Ada', 'Yılmaz', 'company')`, nil, db.CodeCheckViolation, "users_status_chk"},
		{"unknown persona", live + `) VALUES ('active', 'h@example.test', 'h@example.test', 'Ada', 'Yılmaz', 'alien')`, nil, db.CodeCheckViolation, "users_signup_persona_chk"},
		{"unknown locale", live + `, locale) VALUES ('active', 'i@example.test', 'i@example.test', 'Ada', 'Yılmaz', 'company', 'fr-FR')`, nil, db.CodeCheckViolation, "users_locale_chk"},
		{"empty first name", live + `) VALUES ('active', 'j@example.test', 'j@example.test', '', 'Yılmaz', 'company')`, nil, db.CodeCheckViolation, "users_first_name_chk"},
		{"over-long last name", live + `) VALUES ('active', 'k@example.test', 'k@example.test', 'Ada', '` + repeat("y", 101) + `', 'company')`, nil, db.CodeCheckViolation, "users_last_name_chk"},
		{"over-long e-mail", live + `) VALUES ('active', '` + repeat("e", 250) + `@x.test', 'l@example.test', 'Ada', 'Yılmaz', 'company')`, nil, db.CodeCheckViolation, "users_email_chk"},
		{"over-long normalised e-mail", live + `) VALUES ('active', 'o@example.test', '` + repeat("n", 255) + `', 'Ada', 'Yılmaz', 'company')`, nil, db.CodeCheckViolation, "users_email_normalized_chk"},
		{"over-long suspension reason", live + `, suspended_at, suspension_reason) VALUES ('suspended', 'm@example.test', 'm@example.test', 'Ada', 'Yılmaz', 'company', now(), '` + repeat("r", 501) + `')`, nil, db.CodeCheckViolation, "users_suspension_reason_chk"},
		{"app_rw cannot delete a user", `DELETE FROM users WHERE email_normalized = 'a@example.test'`, nil, db.CodeInsufficientPriv, ""},
		{"app_rw cannot truncate users", `TRUNCATE users`, nil, db.CodeInsufficientPriv, ""},
	})

	// Erasing a live account (the tombstone update) is allowed in one step.
	w.exec(`UPDATE users SET status = 'erased', email = NULL, email_normalized = NULL, password_hash = NULL, first_name = NULL,
		last_name = NULL, birth_date = NULL, erased_at = now() WHERE email_normalized = 'b@example.test'`)
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM users WHERE status = 'erased' AND erased_at IS NOT NULL ORDER BY updated_at DESC LIMIT 1`); got != "true" {
		t.Errorf("updated_at was not stamped by koro_touch_updated_at")
	}
	// The freed e-mail can register again.
	expectOK(t, w.d.Pool, []p1aCase{
		{"re-registration with an erased address", live + `) VALUES ('pending_email', 'b@example.test', 'b@example.test', 'Berk', 'Aydın', 'company')`, nil, "", ""},
	})
}

// TestSchema_SessionAndTokenConstraints covers sessions, email_tokens,
// mfa_totp and mfa_recovery_codes (02 §5.2, §5.3, §5.5, §5.6): digest
// lengths and uniqueness, the revoked pairing, the expiry order, the token
// purpose/target pairing, one TOTP secret per user.
func TestSchema_SessionAndTokenConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	u := w.user()
	missing := uuid.NewV7()
	const session = `INSERT INTO sessions (user_id, token_hash, client_kind, idle_expires_at, absolute_expires_at`

	w.exec(session+`) VALUES ($1, $2, 'web', now() + interval '7 days', now() + interval '30 days')`, u, bytes32("s1"))
	expectFail(t, w.d.Pool, []p1aCase{
		{"duplicate session token", session + `) VALUES ($1, $2, 'web', now(), now())`, []any{u, bytes32("s1")}, db.CodeUniqueViolation, "sessions_token_hash_uq"},
		{"short session token digest", session + `) VALUES ($1, '\x0102', 'web', now(), now())`, []any{u}, db.CodeCheckViolation, "sessions_token_hash_chk"},
		{"idle expiry after the absolute expiry", session + `) VALUES ($1, $2, 'web', now() + interval '2 days', now() + interval '1 day')`, []any{u, bytes32("s2")}, db.CodeCheckViolation, "sessions_expiry_chk"},
		{"revoked without a reason", session + `, revoked_at) VALUES ($1, $2, 'web', now(), now(), now())`, []any{u, bytes32("s3")}, db.CodeCheckViolation, "sessions_revoked_chk"},
		{"reason without a revocation", session + `, revoke_reason) VALUES ($1, $2, 'web', now(), now(), 'logout')`, []any{u, bytes32("s4")}, db.CodeCheckViolation, "sessions_revoked_chk"},
		{"unknown revoke reason", session + `, revoked_at, revoke_reason) VALUES ($1, $2, 'web', now(), now(), now(), 'boredom')`, []any{u, bytes32("s5")}, db.CodeCheckViolation, "sessions_revoke_reason_chk"},
		{"unknown client kind", session + `) VALUES ($1, $2, 'tv', now(), now())`, []any{u, bytes32("s6")}, db.CodeCheckViolation, "sessions_client_kind_chk"},
		{"over-long user agent", session + `, user_agent) VALUES ($1, $2, 'web', now(), now(), $3)`, []any{u, bytes32("s7"), repeat("a", 401)}, db.CodeCheckViolation, "sessions_user_agent_chk"},
		{"over-long device label", session + `, device_label) VALUES ($1, $2, 'web', now(), now(), $3)`, []any{u, bytes32("s8"), repeat("d", 101)}, db.CodeCheckViolation, "sessions_device_label_chk"},
		{"session of an unknown user", session + `) VALUES ($1, $2, 'web', now(), now())`, []any{missing, bytes32("s9")}, db.CodeForeignKeyViolation, "sessions_user_id_fkey"},
	})
	w.exec(`UPDATE sessions SET revoked_at = now(), revoke_reason = 'logout' WHERE user_id = $1`, u)
	w.exec(`DELETE FROM sessions WHERE user_id = $1`, u) // expired and revoked sessions are hard-deleted by the sweeper

	const token = `INSERT INTO email_tokens (user_id, purpose, target_email, key_id, token_hash, expires_at`
	w.exec(token+`) VALUES ($1, 'account_email', NULL, 'k1', $2, now() + interval '1 day')`, u, bytes32("t1"))
	w.exec(token+`) VALUES ($1, 'email_change', 'new@example.test', 'k1', $2, now() + interval '1 day')`, u, bytes32("t2"))
	expectFail(t, w.d.Pool, []p1aCase{
		{"email_change without a target", token + `) VALUES ($1, 'email_change', NULL, 'k1', $2, now())`, []any{u, bytes32("t3")}, db.CodeCheckViolation, "email_tokens_target_chk"},
		{"edu_email without a target", token + `) VALUES ($1, 'edu_email', NULL, 'k1', $2, now())`, []any{u, bytes32("t4")}, db.CodeCheckViolation, "email_tokens_target_chk"},
		{"unknown purpose", token + `) VALUES ($1, 'magic', NULL, 'k1', $2, now())`, []any{u, bytes32("t5")}, db.CodeCheckViolation, "email_tokens_purpose_chk"},
		{"duplicate token", token + `) VALUES ($1, 'account_email', NULL, 'k1', $2, now())`, []any{u, bytes32("t1")}, db.CodeUniqueViolation, "email_tokens_token_hash_uq"},
		{"short token digest", token + `) VALUES ($1, 'account_email', NULL, 'k1', '\x00', now())`, []any{u}, db.CodeCheckViolation, "email_tokens_token_hash_chk"},
		{"attempts above ten", `UPDATE email_tokens SET attempts = 11 WHERE user_id = $1`, []any{u}, db.CodeCheckViolation, "email_tokens_attempts_chk"},
		{"over-long target e-mail", token + `) VALUES ($1, 'email_change', $3, 'k1', $2, now())`, []any{u, bytes32("t6"), repeat("e", 255)}, db.CodeCheckViolation, "email_tokens_target_email_chk"},
		{"token of an unknown user", token + `) VALUES ($1, 'account_email', NULL, 'k1', $2, now())`, []any{missing, bytes32("t7")}, db.CodeForeignKeyViolation, "email_tokens_user_id_fkey"},
	})
	w.exec(`UPDATE email_tokens SET consumed_at = now(), attempts = 10 WHERE user_id = $1`, u)

	w.exec(`INSERT INTO mfa_totp (user_id, secret_enc) VALUES ($1, '\x0102')`, u)
	expectFail(t, w.d.Pool, []p1aCase{
		{"second TOTP secret of one user", `INSERT INTO mfa_totp (user_id, secret_enc) VALUES ($1, '\x03')`, []any{u}, db.CodeUniqueViolation, "mfa_totp_pkey"},
		{"TOTP secret of an unknown user", `INSERT INTO mfa_totp (user_id, secret_enc) VALUES ($1, '\x03')`, []any{missing}, db.CodeForeignKeyViolation, "mfa_totp_user_id_fkey"},
		{"TOTP without a secret", `INSERT INTO mfa_totp (user_id) VALUES ($1)`, []any{w.user()}, codeNotNull, ""},
	})
	w.exec(`UPDATE mfa_totp SET confirmed_at = now(), last_used_step = 59000000 WHERE user_id = $1`, u)
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM mfa_totp WHERE user_id = $1`, u); got != "true" {
		t.Errorf("mfa_totp.updated_at was not stamped")
	}
	w.exec(`DELETE FROM mfa_totp WHERE user_id = $1`, u) // disabling MFA is a hard delete

	w.exec(`INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES ($1, $2)`, u, bytes32("r1"))
	expectFail(t, w.d.Pool, []p1aCase{
		{"duplicate recovery code", `INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES ($1, $2)`, []any{u, bytes32("r1")}, db.CodeUniqueViolation, "mfa_recovery_codes_code_hash_uq"},
		{"short recovery code digest", `INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES ($1, '\x01')`, []any{u}, db.CodeCheckViolation, "mfa_recovery_codes_code_hash_chk"},
		{"recovery code of an unknown user", `INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES ($1, $2)`, []any{missing, bytes32("r2")}, db.CodeForeignKeyViolation, "mfa_recovery_codes_user_id_fkey"},
	})
	w.exec(`UPDATE mfa_recovery_codes SET used_at = now() WHERE user_id = $1`, u)
	w.exec(`DELETE FROM mfa_recovery_codes WHERE user_id = $1`, u) // regeneration replaces the set
}

// TestSchema_EduVerificationConstraints covers edu_verifications (02 §5.4):
// the method/evidence pairing, what a verified row must carry, the document
// review rule (and nobody reviews their own), and the partial unique
// indexes (one verified, one pending per user; one .edu.tr address per
// open verification), plus the university FK added by 000037.
func TestSchema_EduVerificationConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	u1, u2, u3, reviewer := w.user(), w.user(), w.user(), w.user()
	uni, _ := w.university()
	file1, file2 := w.file("edu_document"), w.file("edu_document")
	const emailRow = `INSERT INTO edu_verifications (user_id, method, edu_email, edu_email_normalized, email_domain, status, university_id, verified_at, expires_at) VALUES `
	const docRow = `INSERT INTO edu_verifications (user_id, method, document_file_id, status, university_id, verified_at, expires_at, reviewed_by) VALUES `

	expectFail(t, w.d.Pool, []p1aCase{
		{"edu_email without its address", `INSERT INTO edu_verifications (user_id, method) VALUES ($1, 'edu_email')`, []any{u1}, db.CodeCheckViolation, "edu_verifications_email_method_chk"},
		{"address with an incomplete set", `INSERT INTO edu_verifications (user_id, method, edu_email, edu_email_normalized) VALUES ($1, 'edu_email', 'a@x.edu.tr', 'a@x.edu.tr')`, []any{u1}, db.CodeCheckViolation, "edu_verifications_email_method_chk"},
		{"document method without a file", `INSERT INTO edu_verifications (user_id, method) VALUES ($1, 'document')`, []any{u1}, db.CodeCheckViolation, "edu_verifications_document_method_chk"},
		{"file on an e-mail verification", `INSERT INTO edu_verifications (user_id, method, edu_email, edu_email_normalized, email_domain, document_file_id) VALUES ($1, 'edu_email', 'a@x.edu.tr', 'a@x.edu.tr', 'x.edu.tr', $2)`, []any{u1, file1}, db.CodeCheckViolation, "edu_verifications_document_method_chk"},
		{"verified without a university", emailRow + `($1, 'edu_email', 'a@x.edu.tr', 'a@x.edu.tr', 'x.edu.tr', 'verified', NULL, now(), now() + interval '1 year')`, []any{u1}, db.CodeCheckViolation, "edu_verifications_verified_chk"},
		{"verified without an expiry", emailRow + `($1, 'edu_email', 'a@x.edu.tr', 'a@x.edu.tr', 'x.edu.tr', 'verified', $2, now(), NULL)`, []any{u1, uni}, db.CodeCheckViolation, "edu_verifications_verified_chk"},
		{"expiry before verification", emailRow + `($1, 'edu_email', 'a@x.edu.tr', 'a@x.edu.tr', 'x.edu.tr', 'verified', $2, now(), now() - interval '1 day')`, []any{u1, uni}, db.CodeCheckViolation, "edu_verifications_expiry_chk"},
		{"document verified without a reviewer", docRow + `($1, 'document', $2, 'verified', $3, now(), now() + interval '1 year', NULL)`, []any{u1, file1, uni}, db.CodeCheckViolation, "edu_verifications_document_review_chk"},
		{"document rejected without a reviewer", docRow + `($1, 'document', $2, 'rejected', NULL, NULL, NULL, NULL)`, []any{u1, file1}, db.CodeCheckViolation, "edu_verifications_document_review_chk"},
		{"nobody reviews their own document", docRow + `($1, 'document', $2, 'verified', $3, now(), now() + interval '1 year', $1)`, []any{u1, file1, uni}, db.CodeCheckViolation, "edu_verifications_reviewer_chk"},
		{"unknown status", `INSERT INTO edu_verifications (user_id, method, document_file_id, status) VALUES ($1, 'document', $2, 'approved')`, []any{u1, file1}, db.CodeCheckViolation, "edu_verifications_status_chk"},
		{"unknown method", `INSERT INTO edu_verifications (user_id, method) VALUES ($1, 'handshake')`, []any{u1}, db.CodeCheckViolation, "edu_verifications_method_chk"},
		{"verified for an unknown university", emailRow + `($1, 'edu_email', 'z@x.edu.tr', 'z@x.edu.tr', 'x.edu.tr', 'verified', $2, now(), now() + interval '1 year')`, []any{u1, uuid.NewV7()}, db.CodeForeignKeyViolation, "edu_verifications_university_id_fkey"},
		{"over-long .edu.tr address", `INSERT INTO edu_verifications (user_id, method, edu_email, edu_email_normalized, email_domain) VALUES ($1, 'edu_email', $2, 'long@x.edu.tr', 'x.edu.tr')`, []any{u1, repeat("a", 255)}, db.CodeCheckViolation, "edu_verifications_edu_email_chk"},
		{"over-long review note", `INSERT INTO edu_verifications (user_id, method, document_file_id, review_note) VALUES ($1, 'document', $2, $3)`, []any{u1, file1, repeat("n", 501)}, db.CodeCheckViolation, "edu_verifications_review_note_chk"},
		{"verification of an unknown user", `INSERT INTO edu_verifications (user_id, method, document_file_id) VALUES ($1, 'document', $2)`, []any{uuid.NewV7(), file1}, db.CodeForeignKeyViolation, "edu_verifications_user_id_fkey"},
		{"unknown file", `INSERT INTO edu_verifications (user_id, method, document_file_id) VALUES ($1, 'document', $2)`, []any{u1, uuid.NewV7()}, db.CodeForeignKeyViolation, "edu_verifications_document_file_id_fkey"},
		{"unknown reviewer", docRow + `($1, 'document', $2, 'verified', $3, now(), now() + interval '1 year', $4)`, []any{u1, file1, uni, uuid.NewV7()}, db.CodeForeignKeyViolation, "edu_verifications_reviewed_by_fkey"},
	})

	// u1: a pending .edu.tr verification; a second pending one is refused.
	w.exec(emailRow+`($1, 'edu_email', 'Ada@Ogr.X.edu.tr', 'ada@ogr.x.edu.tr', 'ogr.x.edu.tr', 'pending', NULL, NULL, NULL)`, u1)
	expectFail(t, w.d.Pool, []p1aCase{
		{"second pending verification of one user", `INSERT INTO edu_verifications (user_id, method, document_file_id) VALUES ($1, 'document', $2)`, []any{u1, file1}, db.CodeUniqueViolation, "edu_verifications_one_pending_uq"},
		{"one .edu.tr address verifies one account", emailRow + `($1, 'edu_email', 'ADA@ogr.x.edu.tr', 'ada@ogr.x.edu.tr', 'ogr.x.edu.tr', 'pending', NULL, NULL, NULL)`, []any{u2}, db.CodeUniqueViolation, "edu_verifications_email_uq"},
	})
	// u1 verifies; the pending slot is free again and the address stays taken.
	w.exec(`UPDATE edu_verifications SET status = 'verified', verified_at = now(), expires_at = now() + interval '1 year', university_id = $2 WHERE user_id = $1`, u1, uni)
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM edu_verifications WHERE user_id = $1`, u1); got != "true" {
		t.Errorf("edu_verifications.updated_at was not stamped")
	}
	expectFail(t, w.d.Pool, []p1aCase{
		{"address still taken while verified", emailRow + `($1, 'edu_email', 'ada@ogr.x.edu.tr', 'ada@ogr.x.edu.tr', 'ogr.x.edu.tr', 'pending', NULL, NULL, NULL)`, []any{u2}, db.CodeUniqueViolation, "edu_verifications_email_uq"},
		{"second verified row of one user", docRow + `($1, 'document', $2, 'verified', $3, now(), now() + interval '1 year', $4)`, []any{u1, file1, uni, reviewer}, db.CodeUniqueViolation, "edu_verifications_one_verified_uq"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"renewal request while the old row is verified", `INSERT INTO edu_verifications (user_id, method, document_file_id) VALUES ($1, 'document', $2)`, []any{u1, file1}, "", ""},
		{"verified document of another user, reviewed by an admin", docRow + `($1, 'document', $2, 'verified', $3, now(), now() + interval '1 year', $4)`, []any{u3, file2, uni, reviewer}, "", ""},
	})
	// A superseded row releases the address.
	w.exec(`UPDATE edu_verifications SET status = 'superseded' WHERE user_id = $1 AND status = 'verified'`, u1)
	expectOK(t, w.d.Pool, []p1aCase{
		{"address of a superseded verification can be used again", emailRow + `($1, 'edu_email', 'ada@ogr.x.edu.tr', 'ada@ogr.x.edu.tr', 'ogr.x.edu.tr', 'pending', NULL, NULL, NULL)`, []any{u2}, "", ""},
	})
	// BE-018: a decided document row may drop its file (edu_document
	// retention, virus finding); a pending one may not.
	expectFail(t, w.d.Pool, []p1aCase{
		{"pending document without its file", `UPDATE edu_verifications SET document_file_id = NULL WHERE user_id = $1 AND status = 'pending'`, []any{u1}, db.CodeCheckViolation, "edu_verifications_document_method_chk"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"decided document without its file", `UPDATE edu_verifications SET document_file_id = NULL WHERE user_id = $1 AND status = 'verified'`, []any{u3}, "", ""},
	})
}

// TestSchema_PlatformRoleFourEyesCheck (06 H-17, 02 §5.7): granting or
// revoking platform_admin, finance_admin or talent_auditor needs a second
// person. The same person approving → CHECK violation; so are a privileged
// role in effect without an approver, nobody granting/approving/revoking
// their own role, and a revocation that skipped the request. moderator,
// support and dpo are granted and revoked by one admin directly. The
// bootstrap row (no requester) is the only privileged row without an
// approver.
func TestSchema_PlatformRoleFourEyesCheck(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	a, b, target, other := w.user(), w.user(), w.user(), w.user()
	const fourEyes = "platform_role_assignments_four_eyes_chk"

	request := func(role string, requester, user uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.NewV7()
		w.exec(`INSERT INTO platform_role_assignments (id, user_id, role, reason, requested_by) VALUES ($1, $2, $3, 'access review done', $4)`, id, user, role, requester)
		return id
	}

	// Grant: a asks, b approves; a approving their own request is refused.
	req := request("finance_admin", a, target)
	pendingDPO := request("dpo", a, other)
	expectFail(t, w.d.Pool, []p1aCase{
		{"the requester approves their own request", `UPDATE platform_role_assignments SET status = 'active', approved_by = $2, approved_at = now(), granted_at = now() WHERE id = $1`, []any{req, a}, db.CodeCheckViolation, fourEyes},
		{"the beneficiary approves their own role", `UPDATE platform_role_assignments SET status = 'active', approved_by = $2, approved_at = now(), granted_at = now() WHERE id = $1`, []any{req, target}, db.CodeCheckViolation, "platform_role_assignments_self_chk"},
		{"active without an approver", `UPDATE platform_role_assignments SET status = 'active', granted_at = now() WHERE id = $1`, []any{req}, db.CodeCheckViolation, "platform_role_assignments_privileged_chk"},
		{"approval without a time", `UPDATE platform_role_assignments SET status = 'active', approved_by = $2, granted_at = now() WHERE id = $1`, []any{req, b}, db.CodeCheckViolation, "platform_role_assignments_approval_chk"},
		{"active without granted_at", `UPDATE platform_role_assignments SET status = 'active', approved_by = $2, approved_at = now() WHERE id = $1`, []any{req, b}, db.CodeCheckViolation, "platform_role_assignments_granted_chk"},
		{"approver on a still pending row", `UPDATE platform_role_assignments SET approved_by = $2, approved_at = now() WHERE id = $1`, []any{req, b}, db.CodeCheckViolation, "platform_role_assignments_approval_chk"},
		{"granted_at on a pending row", `UPDATE platform_role_assignments SET granted_at = now() WHERE id = $1`, []any{req}, db.CodeCheckViolation, "platform_role_assignments_granted_chk"},
	})
	w.exec(`UPDATE platform_role_assignments SET status = 'active', approved_by = $2, approved_at = now(), granted_at = now() WHERE id = $1`, req, b)

	// Nobody asks for their own role; a role without a reason is refused.
	expectFail(t, w.d.Pool, []p1aCase{
		{"self grant", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by) VALUES ($1, 'platform_admin', 'give me the keys', $1)`, []any{a}, db.CodeCheckViolation, "platform_role_assignments_self_chk"},
		{"short reason", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by) VALUES ($1, 'support', 'ok', $2)`, []any{target, a}, db.CodeCheckViolation, "platform_role_assignments_reason_chk"},
		{"unknown role", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by) VALUES ($1, 'superuser', 'because I can', $2)`, []any{target, a}, db.CodeCheckViolation, "platform_role_assignments_role_chk"},
		{"unknown status", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by, status) VALUES ($1, 'support', 'because I can', $2, 'maybe')`, []any{target, a}, db.CodeCheckViolation, "platform_role_assignments_status_chk"},
		{"requester that does not exist", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by) VALUES ($1, 'support', 'because I can', $2)`, []any{target, uuid.NewV7()}, db.CodeForeignKeyViolation, "platform_role_assignments_requested_by_fkey"},
		{"role for a user that does not exist", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by) VALUES ($1, 'support', 'because I can', $2)`, []any{uuid.NewV7(), a}, db.CodeForeignKeyViolation, "platform_role_assignments_user_id_fkey"},
		{"approver that does not exist", `UPDATE platform_role_assignments SET status = 'active', approved_by = $2, approved_at = now(), granted_at = now() WHERE id = $1`, []any{pendingDPO, uuid.NewV7()}, db.CodeForeignKeyViolation, "platform_role_assignments_approved_by_fkey"},
		{"rejecter that does not exist", `UPDATE platform_role_assignments SET status = 'rejected', rejected_by = $2, rejected_at = now() WHERE id = $1`, []any{pendingDPO, uuid.NewV7()}, db.CodeForeignKeyViolation, "platform_role_assignments_rejected_by_fkey"},
		{"no requester without the bootstrap reason", `INSERT INTO platform_role_assignments (user_id, role, reason, status, granted_at) VALUES ($1, 'platform_admin', 'trust me', 'active', now())`, []any{other}, db.CodeCheckViolation, "platform_role_assignments_bootstrap_chk"},
		{"privileged role in effect without an approver", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by, status, granted_at) VALUES ($1, 'talent_auditor', 'fast lane', $2, 'active', now())`, []any{other, a}, db.CodeCheckViolation, "platform_role_assignments_privileged_chk"},
		{"one open assignment per user and role", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by) VALUES ($1, 'finance_admin', 'second request', $2)`, []any{target, b}, db.CodeUniqueViolation, "platform_role_assignments_open_uq"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"the same user may hold another role", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by) VALUES ($1, 'talent_auditor', 'second role', $2)`, []any{target, a}, "", ""},
		{"moderator granted by one admin, no approver", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by, status, granted_at) VALUES ($1, 'moderator', 'new moderator', $2, 'active', now())`, []any{other, a}, "", ""},
		{"support granted by one admin", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by, status, granted_at) VALUES ($1, 'support', 'new support', $2, 'active', now())`, []any{other, b}, "", ""},
		{"bootstrap platform_admin: no requester, no approver", `INSERT INTO platform_role_assignments (user_id, role, reason, status, granted_at) VALUES ($1, 'platform_admin', 'bootstrap', 'active', now())`, []any{a}, "", ""},
		{"second bootstrap platform_admin", `INSERT INTO platform_role_assignments (user_id, role, reason, status, granted_at) VALUES ($1, 'platform_admin', 'bootstrap', 'active', now())`, []any{b}, "", ""},
	})

	// Revocation of a privileged role: a asks, the same person cannot approve.
	expectFail(t, w.d.Pool, []p1aCase{
		{"revoke without a request", `UPDATE platform_role_assignments SET status = 'revoked', revoke_reason = 'left the team', revoked_by = $2, revoked_at = now() WHERE id = $1`, []any{req, b}, db.CodeCheckViolation, "platform_role_assignments_revoke_four_eyes_chk"},
		{"revoke request without a reason", `UPDATE platform_role_assignments SET status = 'revoke_pending', revoke_requested_by = $2, revoke_requested_at = now() WHERE id = $1`, []any{req, a}, db.CodeCheckViolation, "platform_role_assignments_revoked_chk"},
		{"revoke reason too short", `UPDATE platform_role_assignments SET status = 'revoke_pending', revoke_reason = 'no', revoke_requested_by = $2, revoke_requested_at = now() WHERE id = $1`, []any{req, a}, db.CodeCheckViolation, "platform_role_assignments_revoke_reason_chk"},
		{"revoke requester that does not exist", `UPDATE platform_role_assignments SET status = 'revoke_pending', revoke_reason = 'left the team', revoke_requested_by = $2, revoke_requested_at = now() WHERE id = $1`, []any{req, uuid.NewV7()}, db.CodeForeignKeyViolation, "platform_role_assignments_revoke_requested_by_fkey"},
		{"revoke request without a requester", `UPDATE platform_role_assignments SET status = 'revoke_pending', revoke_reason = 'left the team' WHERE id = $1`, []any{req}, db.CodeCheckViolation, "platform_role_assignments_revoke_request_chk"},
		{"nobody revokes their own role", `UPDATE platform_role_assignments SET status = 'revoke_pending', revoke_reason = 'self service', revoke_requested_by = $2, revoke_requested_at = now() WHERE id = $1`, []any{req, target}, db.CodeCheckViolation, "platform_role_assignments_self_chk"},
	})
	w.exec(`UPDATE platform_role_assignments SET status = 'revoke_pending', revoke_reason = 'left the team', revoke_requested_by = $2, revoke_requested_at = now() WHERE id = $1`, req, a)
	expectFail(t, w.d.Pool, []p1aCase{
		{"the requester approves their own revocation", `UPDATE platform_role_assignments SET status = 'revoked', revoked_by = $2, revoked_at = now() WHERE id = $1`, []any{req, a}, db.CodeCheckViolation, "platform_role_assignments_revoke_four_eyes_chk"},
		{"revoked without revoked_at", `UPDATE platform_role_assignments SET status = 'revoked', revoked_by = $2 WHERE id = $1`, []any{req, b}, db.CodeCheckViolation, "platform_role_assignments_revoked_chk"},
		{"the open slot is still taken while revoke_pending", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by) VALUES ($1, 'finance_admin', 'again', $2)`, []any{target, b}, db.CodeUniqueViolation, "platform_role_assignments_open_uq"},
	})
	w.exec(`UPDATE platform_role_assignments SET status = 'revoked', revoked_by = $2, revoked_at = now() WHERE id = $1`, req, b)
	// Revoked: the slot is free, the history row stays.
	again := request("finance_admin", b, target)
	if again == req {
		t.Fatal("same id")
	}
	// A refused request is a 'rejected' row with who and when.
	expectFail(t, w.d.Pool, []p1aCase{
		{"rejected without who and when", `UPDATE platform_role_assignments SET status = 'rejected' WHERE id = $1`, []any{again}, db.CodeCheckViolation, "platform_role_assignments_rejected_chk"},
		{"rejected_by without rejected_at", `UPDATE platform_role_assignments SET rejected_by = $2 WHERE id = $1`, []any{again, a}, db.CodeCheckViolation, "platform_role_assignments_rejected_chk"},
	})
	w.exec(`UPDATE platform_role_assignments SET status = 'rejected', rejected_by = $2, rejected_at = now() WHERE id = $1`, again, a)
	expectOK(t, w.d.Pool, []p1aCase{
		{"a rejected request frees the slot", `INSERT INTO platform_role_assignments (user_id, role, reason, requested_by) VALUES ($1, 'finance_admin', 'third try', $2)`, []any{target, b}, "", ""},
	})

	// moderator/support/dpo: one admin revokes directly.
	expectFail(t, w.d.Pool, []p1aCase{
		{"revoker that does not exist", `UPDATE platform_role_assignments SET status = 'revoked', revoke_reason = 'role ended', revoked_by = $2, revoked_at = now()
			WHERE user_id = $1 AND role = 'moderator'`, []any{other, uuid.NewV7()}, db.CodeForeignKeyViolation, "platform_role_assignments_revoked_by_fkey"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"moderator revoked by the single admin", `UPDATE platform_role_assignments SET status = 'revoked', revoke_reason = 'role ended', revoked_by = $2, revoked_at = now()
			WHERE user_id = $1 AND role = 'moderator'`, []any{other, a}, "", ""},
	})

	// Which rows are in effect: only active and revoke_pending ones.
	if n := w.count(`SELECT count(*) FROM platform_role_assignments WHERE status IN ('active','revoke_pending')`); n != 3 {
		t.Errorf("roles in effect = %d, want 3 (two bootstrap platform_admins and support; the pending requests are not in effect)", n)
	}
	if n := w.count(`SELECT count(*) FROM platform_role_assignments WHERE status = 'pending'`); n != 3 {
		t.Errorf("pending requests = %d, want 3 (talent_auditor and finance_admin of target, dpo of other)", n)
	}
	// Hard delete after the retention period (role_history).
	w.exec(`DELETE FROM platform_role_assignments WHERE status IN ('revoked','rejected')`)
}
