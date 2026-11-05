//go:build integration

package migrations_test

import (
	"bytes"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_WebPushSubscriptions (BE-173, migration 000175): a browser
// subscription is an https endpoint (unique by its digest) with a 65-byte
// key and a 16-byte secret of a real user; app_rw reads, writes and deletes
// it; the web_push preference channel exists. The down migration drops the
// table and the web_push preferences, the up one brings them back.
func TestSchema_WebPushSubscriptions(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	const ins = `INSERT INTO web_push_subscriptions (user_id, endpoint, endpoint_sha256, p256dh, auth) VALUES ($1, $2, $3, $4, $5)`
	key, secret := bytes.Repeat([]byte{4}, 65), bytes.Repeat([]byte{7}, 16)
	check := func(t *testing.T) {
		t.Helper()
		u := w.user()
		ep := "https://fcm.googleapis.com/fcm/send/" + w.n()
		expectOK(t, w.d.Pool, []p1aCase{
			{name: "a subscription", sql: ins, args: []any{u, ep, sha(ep), key, secret}},
			{name: "web_push preference", sql: `INSERT INTO notification_preferences (user_id, category, channel, enabled)
				VALUES ($1, 'event_updates', 'web_push', false)`, args: []any{u}},
			{name: "refresh", sql: `UPDATE web_push_subscriptions SET last_seen_at = now() WHERE user_id = $1`, args: []any{u}},
		})
		chk := db.CodeCheckViolation
		expectFail(t, w.d.Pool, []p1aCase{
			{name: "same endpoint", sql: ins, args: []any{w.user(), ep, sha(ep), key, secret}, state: db.CodeUniqueViolation,
				constraint: "web_push_subscriptions_endpoint_sha256_uq"},
			{name: "http endpoint", sql: ins, args: []any{u, "http://fcm.googleapis.com/x", sha("h1"), key, secret}, state: chk,
				constraint: "web_push_subscriptions_endpoint_chk"},
			{name: "short digest", sql: ins, args: []any{u, ep + "/2", []byte("short"), key, secret}, state: chk,
				constraint: "web_push_subscriptions_endpoint_sha256_chk"},
			{name: "short key", sql: ins, args: []any{u, ep + "/3", sha("k3"), key[:33], secret}, state: chk,
				constraint: "web_push_subscriptions_p256dh_chk"},
			{name: "short secret", sql: ins, args: []any{u, ep + "/4", sha("k4"), key, secret[:8]}, state: chk,
				constraint: "web_push_subscriptions_auth_chk"},
			{name: "unknown user", sql: ins, args: []any{uuid.NewV7(), ep + "/5", sha("k5"), key, secret}, state: db.CodeForeignKeyViolation,
				constraint: "web_push_subscriptions_user_id_fkey"},
			{name: "unknown channel", sql: `INSERT INTO notification_preferences (user_id, category, channel, enabled)
				VALUES ($1, 'event_updates', 'sms', false)`, args: []any{u}, state: chk, constraint: "notification_preferences_channel_chk"},
		})
		expectOK(t, w.d.Pool, []p1aCase{{name: "delete (gone, erasure)", sql: `DELETE FROM web_push_subscriptions WHERE user_id = $1`, args: []any{u}}})
		// A web_push preference waits for the down migration.
		w.exec(`INSERT INTO notification_preferences (user_id, category, channel, enabled) VALUES ($1, 'moderation', 'web_push', false)`, w.user())
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000175_create_web_push_subscriptions.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'web_push_subscriptions'`) +
		w.count(`SELECT count(*) FROM notification_preferences WHERE channel = 'web_push'`); n != 0 {
		t.Fatalf("down left %d objects of 000175", n)
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000175_create_web_push_subscriptions.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
