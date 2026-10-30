package identity_test

import (
	"testing"

	"koro/internal/platform/db/dbtest"
)

// TestStore_ReachedThroughNarrowViews keeps the BE-148 split (05 §2.2): a
// service file calls the store only through the view of its aggregate
// (s.users(), s.sessions(), s.emailTokens(), s.roles(), s.mfa(),
// s.accountActions(), s.edu(); the bootstrap's b.users(), b.roles(),
// b.sessions(); store.go), the retention sweeper through the views it holds
// (retention.go). Only the accessors themselves and the KVKK provider
// (privacy.go: export and erasure cover every row of a data subject) hold
// the whole Store.
func TestStore_ReachedThroughNarrowViews(t *testing.T) {
	t.Parallel()
	dbtest.CheckStoreViews(t, "store.go", "privacy.go")
}
