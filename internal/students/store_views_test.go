package students_test

import (
	"testing"

	"koro/internal/platform/db/dbtest"
)

// TestStore_ReachedThroughNarrowViews keeps the BE-142 split (05 §2.2): a
// service file calls the store only through the view of its aggregate
// (s.profiles(), s.privacy(), s.sections(), s.cvs(), s.matches(),
// s.grants(); store.go). Only the accessors themselves and the KVKK
// provider (privacy.go: export and erasure cover every table) hold the
// whole Store.
func TestStore_ReachedThroughNarrowViews(t *testing.T) {
	t.Parallel()
	dbtest.CheckStoreViews(t, "store.go", "privacy.go")
}
