//go:build integration

package migrations_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// TestSchema_NotificationDeliveryConstraints covers notification_deliveries
// (02 §6.1): a recipient is required (user or blind index), only e-mail may
// go to a recipient without an account, the dedupe key and the provider
// message id are unique, and the template/category/status vocabularies.
func TestSchema_NotificationDeliveryConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	u := w.user()
	const cols = `INSERT INTO notification_deliveries (user_id, recipient_bidx, channel, category, template, template_version, dedupe_key, provider, provider_message_id, status) VALUES `

	expectOK(t, w.d.Pool, []p1aCase{
		{"e-mail to a user", cols + `($1, NULL, 'email', 'transactional', 'identity.verify_email', 1, 'd-1', NULL, NULL, 'queued')`, []any{u}, "", ""},
		{"e-mail to an address without an account (blind index only)", cols + `(NULL, $1, 'email', 'transactional', 'orgs.invitation', 1, 'd-2', NULL, NULL, 'queued')`, []any{bytes32("addr")}, "", ""},
		{"in-app to a user", cols + `($1, NULL, 'in_app', 'transactional', 'events.reminder', 2, 'd-3', NULL, NULL, 'sent')`, []any{u}, "", ""},
		{"two deliveries without a provider message id", cols + `($1, NULL, 'email', 'commercial', 'feed.digest', 1, 'd-4', 'smtp', NULL, 'queued')`, []any{u}, "", ""},
		{"provider message id", cols + `($1, NULL, 'email', 'commercial', 'feed.digest', 1, 'd-5', 'smtp', 'msg-1', 'sent')`, []any{u}, "", ""},
		{"same message id at another provider", cols + `($1, NULL, 'email', 'commercial', 'feed.digest', 1, 'd-6', 'ses', 'msg-1', 'sent')`, []any{u}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"no recipient at all", cols + `(NULL, NULL, 'email', 'transactional', 'identity.verify_email', 1, 'x-1', NULL, NULL, 'queued')`, nil, db.CodeCheckViolation, "notification_deliveries_recipient_chk"},
		{"push to a recipient without an account", cols + `(NULL, $1, 'push', 'transactional', 'identity.verify_email', 1, 'x-2', NULL, NULL, 'queued')`, []any{bytes32("addr")}, db.CodeCheckViolation, "notification_deliveries_channel_user_chk"},
		{"in-app to a recipient without an account", cols + `(NULL, $1, 'in_app', 'transactional', 'identity.verify_email', 1, 'x-3', NULL, NULL, 'queued')`, []any{bytes32("addr")}, db.CodeCheckViolation, "notification_deliveries_channel_user_chk"},
		{"duplicate dedupe key", cols + `($1, NULL, 'email', 'transactional', 'identity.verify_email', 1, 'd-1', NULL, NULL, 'queued')`, []any{u}, db.CodeUniqueViolation, "notification_deliveries_dedupe_key_uq"},
		{"duplicate provider message id", cols + `($1, NULL, 'email', 'commercial', 'feed.digest', 1, 'x-4', 'smtp', 'msg-1', 'sent')`, []any{u}, db.CodeUniqueViolation, "notification_deliveries_provider_uq"},
		{"over-long dedupe key", cols + `($1, NULL, 'email', 'transactional', 'identity.verify_email', 1, '` + repeat("k", 201) + `', NULL, NULL, 'queued')`, []any{u}, db.CodeCheckViolation, "notification_deliveries_dedupe_key_chk"},
		{"short blind index", cols + `(NULL, '\x01', 'email', 'transactional', 'identity.verify_email', 1, 'x-5', NULL, NULL, 'queued')`, nil, db.CodeCheckViolation, "notification_deliveries_recipient_bidx_chk"},
		{"template with capitals", cols + `($1, NULL, 'email', 'transactional', 'Identity.Verify', 1, 'x-6', NULL, NULL, 'queued')`, []any{u}, db.CodeCheckViolation, "notification_deliveries_template_chk"},
		{"template with a dash", cols + `($1, NULL, 'email', 'transactional', 'identity.verify-email', 1, 'x-7', NULL, NULL, 'queued')`, []any{u}, db.CodeCheckViolation, "notification_deliveries_template_chk"},
		{"template version zero", cols + `($1, NULL, 'email', 'transactional', 'identity.verify_email', 0, 'x-8', NULL, NULL, 'queued')`, []any{u}, db.CodeCheckViolation, "notification_deliveries_template_version_chk"},
		{"unknown channel", cols + `($1, NULL, 'sms', 'transactional', 'identity.verify_email', 1, 'x-9', NULL, NULL, 'queued')`, []any{u}, db.CodeCheckViolation, "notification_deliveries_channel_chk"},
		{"unknown category", cols + `($1, NULL, 'email', 'marketing', 'identity.verify_email', 1, 'x-10', NULL, NULL, 'queued')`, []any{u}, db.CodeCheckViolation, "notification_deliveries_category_chk"},
		{"unknown status", cols + `($1, NULL, 'email', 'transactional', 'identity.verify_email', 1, 'x-11', NULL, NULL, 'vanished')`, []any{u}, db.CodeCheckViolation, "notification_deliveries_status_chk"},
		{"negative attempts", `INSERT INTO notification_deliveries (user_id, channel, category, template, template_version, dedupe_key, attempts) VALUES ($1, 'email', 'transactional', 'identity.verify_email', 1, 'x-13', -1)`, []any{u}, db.CodeCheckViolation, "notification_deliveries_attempts_chk"},
		{"over-long last error", `INSERT INTO notification_deliveries (user_id, channel, category, template, template_version, dedupe_key, last_error) VALUES ($1, 'email', 'transactional', 'identity.verify_email', 1, 'x-14', $2)`, []any{u, repeat("e", 2001)}, db.CodeCheckViolation, "notification_deliveries_last_error_chk"},
		{"delivery of an unknown user", cols + `($1, NULL, 'email', 'transactional', 'identity.verify_email', 1, 'x-12', NULL, NULL, 'queued')`, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "notification_deliveries_user_id_fkey"},
	})
	w.exec(`UPDATE notification_deliveries SET status = 'delivered', attempts = 1, delivered_at = now() WHERE dedupe_key = 'd-1'`)
	if got := queryString(t, w.d, `SELECT (updated_at > created_at)::text FROM notification_deliveries WHERE dedupe_key = 'd-1'`); got != "true" {
		t.Errorf("notification_deliveries.updated_at was not stamped")
	}
	w.exec(`DELETE FROM notification_deliveries WHERE dedupe_key = 'd-1'`) // retention: hard delete after 12 months
}

// TestSchema_EmailSuppressionAndPushTokenConstraints covers
// email_suppressions and push_tokens (02 §6.2, §6.3), including the
// ON DELETE SET NULL of push_tokens.session_id.
func TestSchema_EmailSuppressionAndPushTokenConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	u := w.user()

	w.exec(`INSERT INTO email_suppressions (email_bidx, reason, source) VALUES ($1, 'hard_bounce', 'provider_webhook')`, bytes32("bounced"))
	expectFail(t, w.d.Pool, []p1aCase{
		{"suppress the same address twice", `INSERT INTO email_suppressions (email_bidx, reason, source) VALUES ($1, 'complaint', 'admin')`, []any{bytes32("bounced")}, db.CodeUniqueViolation, "email_suppressions_pkey"},
		{"short blind index", `INSERT INTO email_suppressions (email_bidx, reason, source) VALUES ('\x0102', 'manual', 'admin')`, nil, db.CodeCheckViolation, "email_suppressions_email_bidx_chk"},
		{"unknown reason", `INSERT INTO email_suppressions (email_bidx, reason, source) VALUES ($1, 'dislike', 'admin')`, []any{bytes32("x1")}, db.CodeCheckViolation, "email_suppressions_reason_chk"},
		{"unknown source", `INSERT INTO email_suppressions (email_bidx, reason, source) VALUES ($1, 'manual', 'rumour')`, []any{bytes32("x2")}, db.CodeCheckViolation, "email_suppressions_source_chk"},
	})

	sid := uuid.NewV7()
	w.exec(`INSERT INTO sessions (id, user_id, token_hash, client_kind, idle_expires_at, absolute_expires_at)
		VALUES ($1, $2, $3, 'mobile', now() + interval '1 day', now() + interval '2 days')`, sid, u, bytes32("sess"))
	const cols = `INSERT INTO push_tokens (user_id, session_id, platform, token, token_sha256, app_version) VALUES `
	w.exec(cols+`($1, $2, 'ios', 'apns-token-1', $3, '1.0.0')`, u, sid, bytes32("tok1"))
	expectFail(t, w.d.Pool, []p1aCase{
		{"the same device token twice", cols + `($1, NULL, 'ios', 'apns-token-1', $2, NULL)`, []any{u, bytes32("tok1")}, db.CodeUniqueViolation, "push_tokens_token_sha256_uq"},
		{"unknown platform", cols + `($1, NULL, 'blackberry', 't', $2, NULL)`, []any{u, bytes32("tok2")}, db.CodeCheckViolation, "push_tokens_platform_chk"},
		{"over-long token", cols + `($1, NULL, 'android', $3, $2, NULL)`, []any{u, bytes32("tok3"), repeat("t", 4097)}, db.CodeCheckViolation, "push_tokens_token_chk"},
		{"short token digest", cols + `($1, NULL, 'android', 't', '\x01', NULL)`, []any{u}, db.CodeCheckViolation, "push_tokens_token_sha256_chk"},
		{"over-long app version", cols + `($1, NULL, 'android', 't', $2, $3)`, []any{u, bytes32("tok4"), repeat("v", 41)}, db.CodeCheckViolation, "push_tokens_app_version_chk"},
		{"token of an unknown user", cols + `($1, NULL, 'android', 't', $2, NULL)`, []any{uuid.NewV7(), bytes32("tok5")}, db.CodeForeignKeyViolation, "push_tokens_user_id_fkey"},
		{"token of an unknown session", cols + `($1, $3, 'android', 't', $2, NULL)`, []any{u, bytes32("tok6"), uuid.NewV7()}, db.CodeForeignKeyViolation, "push_tokens_session_id_fkey"},
	})
	// Deleting the session keeps the token and clears the link (ON DELETE SET NULL).
	w.exec(`DELETE FROM sessions WHERE id = $1`, sid)
	if n := w.count(`SELECT count(*) FROM push_tokens WHERE user_id = $1 AND session_id IS NULL`, u); n != 1 {
		t.Errorf("push tokens without a session after the session was deleted = %d, want 1", n)
	}
}

// TestSchema_InAppNotificationConstraints covers in_app_notifications
// (02 §6.4) with the link_path rule: an in-app relative path only. A path
// starting with '//' is a protocol-relative URL and is refused (02 fix), as
// are absolute URLs and schemes. The org FK is added by 000032.
func TestSchema_InAppNotificationConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	u := w.user()
	org := w.org("club")
	const cols = `INSERT INTO in_app_notifications (user_id, kind, title, link_path, source_org_id) VALUES `

	for _, link := range []string{"/", "/me/notifications", "/events/0192f5c4?tab=forms&x=1", "/orgs/my-club/posts.json", "/a_b-c/d"} {
		if _, err := w.d.Pool.Exec(t.Context(), cols+`($1, 'events.reminder', 'Başlık', $2, NULL)`, u, link); err != nil {
			t.Errorf("link_path %q was refused: %v", link, err)
		}
	}
	var refused []p1aCase
	for _, link := range []string{
		"//evil.example/login", "//", "https://evil.example", "http://evil.example/x", "javascript:alert(1)", "mailto:a@b.c",
		"events/1", "/path with space", "/a\\b", "/<script>", "/a#frag", "",
	} {
		refused = append(refused, p1aCase{"link_path " + link, cols + `($1, 'events.reminder', 'Başlık', $2, NULL)`, []any{u, link}, db.CodeCheckViolation, "in_app_notifications_link_path_chk"})
	}
	expectFail(t, w.d.Pool, refused)
	expectOK(t, w.d.Pool, []p1aCase{
		{"no link at all", cols + `($1, 'events.reminder', 'Başlık', NULL, NULL)`, []any{u}, "", ""},
		{"notification from an org", cols + `($1, 'feed.new_post', 'Başlık', '/orgs/x', $2)`, []any{u, org}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"kind with capitals", cols + `($1, 'Events.Reminder', 'Başlık', NULL, NULL)`, []any{u}, db.CodeCheckViolation, "in_app_notifications_kind_chk"},
		{"over-long title", cols + `($1, 'events.reminder', '` + repeat("t", 141) + `', NULL, NULL)`, []any{u}, db.CodeCheckViolation, "in_app_notifications_title_chk"},
		{"over-long body", `INSERT INTO in_app_notifications (user_id, kind, title, body) VALUES ($1, 'events.reminder', 't', $2)`, []any{u, repeat("b", 501)}, db.CodeCheckViolation, "in_app_notifications_body_chk"},
		{"notification of an unknown user", cols + `($1, 'events.reminder', 'Başlık', NULL, NULL)`, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "in_app_notifications_user_id_fkey"},
		{"notification from an unknown org", cols + `($1, 'feed.new_post', 'Başlık', NULL, $2)`, []any{u, uuid.NewV7()}, db.CodeForeignKeyViolation, "in_app_notifications_source_org_id_fkey"},
		{"app_rw cannot delete an org", `DELETE FROM organizations WHERE id = $1`, []any{org}, db.CodeInsufficientPriv, ""},
	})
	w.exec(`UPDATE in_app_notifications SET read_at = now() WHERE user_id = $1`, u) // read_at is the one mutable column
	if n := w.count(`SELECT count(*) FROM in_app_notifications WHERE user_id = $1 AND read_at IS NULL`, u); n != 0 {
		t.Errorf("unread after marking read = %d", n)
	}
}

// TestSchema_NotificationPreferenceConstraints covers
// notification_preferences (02 §6.5): one row per user, category and
// channel; security notifications cannot be switched off.
func TestSchema_NotificationPreferenceConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	u := w.user()
	const cols = `INSERT INTO notification_preferences (user_id, category, channel, enabled) VALUES `
	expectOK(t, w.d.Pool, []p1aCase{
		{"switch e-mail of event updates off", cols + `($1, 'event_updates', 'email', false)`, []any{u}, "", ""},
		{"security stays on", cols + `($1, 'security', 'email', true)`, []any{u}, "", ""},
		{"same category on another channel", cols + `($1, 'event_updates', 'push', false)`, []any{u}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"switch security off", cols + `($1, 'security', 'push', false)`, []any{u}, db.CodeCheckViolation, "notification_preferences_security_chk"},
		{"switch security off by update", `UPDATE notification_preferences SET enabled = false WHERE user_id = $1 AND category = 'security'`, []any{u}, db.CodeCheckViolation, "notification_preferences_security_chk"},
		{"duplicate preference", cols + `($1, 'event_updates', 'email', true)`, []any{u}, db.CodeUniqueViolation, "notification_preferences_pkey"},
		{"unknown category", cols + `($1, 'promotions', 'email', true)`, []any{u}, db.CodeCheckViolation, "notification_preferences_category_chk"},
		{"unknown channel", cols + `($1, 'event_updates', 'sms', true)`, []any{u}, db.CodeCheckViolation, "notification_preferences_channel_chk"},
		{"preference of an unknown user", cols + `($1, 'event_updates', 'email', true)`, []any{uuid.NewV7()}, db.CodeForeignKeyViolation, "notification_preferences_user_id_fkey"},
		{"unset flag", `INSERT INTO notification_preferences (user_id, category, channel) VALUES ($1, 'moderation', 'email')`, []any{u}, codeNotNull, ""},
	})
	// The upsert of the store: updated_at moves with it.
	var before, after time.Time
	scan := func(dst *time.Time) {
		t.Helper()
		if err := w.d.Pool.QueryRow(t.Context(), `SELECT updated_at FROM notification_preferences
			WHERE user_id = $1 AND category = 'event_updates' AND channel = 'email'`, u).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	scan(&before)
	w.exec(`INSERT INTO notification_preferences (user_id, category, channel, enabled) VALUES ($1, 'event_updates', 'email', true)
		ON CONFLICT (user_id, category, channel) DO UPDATE SET enabled = EXCLUDED.enabled`, u)
	scan(&after)
	if !after.After(before) {
		t.Errorf("updated_at did not move on update: %v, %v", before, after)
	}
	if n := w.count(`SELECT count(*) FROM notification_preferences WHERE user_id = $1 AND enabled`, u); n != 2 {
		t.Errorf("enabled preferences = %d, want 2 (security and the re-enabled e-mail)", n)
	}
}
