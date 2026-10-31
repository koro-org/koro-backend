//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
)

// Tests of 000162 (BE-153, Q-36): the opaque one-click unsubscribe links.

// TestSchema_UnsubscribeLinks: a link is a 32-byte token digest, unique,
// of a commercial_email consent of a real user, expiring after it was
// made; app_rw reads, inserts and deletes (the erasure, the cleanup) but
// never changes a link; the retention policy is a 7-day job policy.
func TestSchema_UnsubscribeLinks(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	u := w.user()
	ins := `INSERT INTO unsubscribe_links (token_hash, user_id, purpose_code, created_at, expires_at)
		VALUES ($1, $2, $3, now(), now() + interval '365 days')`
	expectOK(t, w.d.Pool, []p1aCase{{name: "a link", sql: ins, args: []any{sha("token-1"), u, "commercial_email"}}})
	chk := db.CodeCheckViolation
	expectFail(t, w.d.Pool, []p1aCase{
		{name: "same digest", sql: ins, args: []any{sha("token-1"), w.user(), "commercial_email"}, state: db.CodeUniqueViolation,
			constraint: "unsubscribe_links_token_hash_uq"},
		{name: "short digest", sql: ins, args: []any{[]byte("short"), u, "commercial_email"}, state: chk, constraint: "unsubscribe_links_token_hash_chk"},
		{name: "push", sql: ins, args: []any{sha("token-2"), u, "commercial_push"}, state: chk, constraint: "unsubscribe_links_purpose_code_chk"},
		{name: "unknown user", sql: ins, args: []any{sha("token-3"), uuid.NewV7(), "commercial_email"}, state: db.CodeForeignKeyViolation,
			constraint: "unsubscribe_links_user_id_fkey"},
		{name: "expired at birth", sql: `INSERT INTO unsubscribe_links (token_hash, user_id, purpose_code, created_at, expires_at)
			VALUES ($1, $2, 'commercial_email', now(), now())`, args: []any{sha("token-4"), u}, state: chk, constraint: "unsubscribe_links_expiry_chk"},
		{name: "update", sql: `UPDATE unsubscribe_links SET expires_at = expires_at + interval '1 day' WHERE user_id = $1`, args: []any{u},
			state: db.CodeInsufficientPriv},
	})
	expectOK(t, w.d.Pool, []p1aCase{{name: "delete (erasure, cleanup)", sql: `DELETE FROM unsubscribe_links WHERE user_id = $1`, args: []any{u}}})
	if got := queryString(t, w.d, `SELECT retention::text || ' ' || anchor || ' ' || method || ' ' || target || ' ' || owner_module || ' ' ||
		legal_approved::text || ' ' || enabled::text FROM retention_policies WHERE code = 'unsubscribe_link'`); got !=
		"7 days expires_at hard_delete unsubscribe_links consent false true" {
		t.Fatalf("unsubscribe_link policy = %s", got)
	}
}
