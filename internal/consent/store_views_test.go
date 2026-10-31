package consent_test

import (
	"testing"

	"koro/internal/platform/db/dbtest"
)

// TestStore_ReachedThroughNarrowViews keeps the BE-156 split (05 §2.2): a
// service file calls the store only through the view of its aggregate
// (s.purposes(), s.notices(), s.consents(), s.submissions(), s.iysState(),
// s.terms(), s.unsubscribeLinks(); store.go), the retention sweeper through
// the purges it holds (retention.go). Only the accessors themselves and the
// KVKK provider (privacy.go: export and erasure cover every row of a data
// subject) hold the whole Store.
func TestStore_ReachedThroughNarrowViews(t *testing.T) {
	t.Parallel()
	dbtest.CheckStoreViews(t, "store.go", "privacy.go")
}
