package messaging_test

import (
	"testing"

	"koro/internal/platform/db/dbtest"
)

// TestStore_ReachedThroughNarrowViews keeps the BE-162 split (05 §2.2): a
// service file calls the store only through the view of its aggregate
// (s.requests(), s.blocks(), s.conversations(); store.go), the retention
// sweeper through the same views (retention.go). Only the accessors
// themselves and the KVKK provider (privacy.go: export and erasure cover
// every row of a data subject) hold the whole Store.
func TestStore_ReachedThroughNarrowViews(t *testing.T) {
	t.Parallel()
	dbtest.CheckStoreViews(t, "store.go", "privacy.go")
}
