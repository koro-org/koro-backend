//go:build integration

package migrations_test

import (
	"strings"
	"testing"
)

// TestSchema_SessionsUserHistoryIndex (BE-209, migration 000186): the new
// sign-in alert reads an account's newest session rows, revoked ones
// included, through a full (user_id, id) index.
func TestSchema_SessionsUserHistoryIndex(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	def := queryString(t, w.d, `SELECT indexdef FROM pg_indexes WHERE indexname = 'sessions_user_history_idx'`)
	if !strings.Contains(def, "ON public.sessions") || !strings.Contains(def, "(user_id, id)") || strings.Contains(def, "WHERE") {
		t.Fatalf("indexdef = %q", def)
	}
}
