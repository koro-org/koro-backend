package orgs_test

import (
	"testing"

	"koro/internal/platform/db/dbtest"
)

// TestStore_ReachedThroughNarrowViews keeps the BE-147 split (05 §2.2): a
// service file calls the store only through the view of its aggregate
// (s.orgs(), s.companies(), s.members(), s.directory(), s.sweeps(),
// s.invitations(); store.go), the retention sweeper through the purge it
// holds (retention.go). Only the accessors themselves and the KVKK
// provider (privacy.go: export and erasure cover every row of a data
// subject) hold the whole Store.
func TestStore_ReachedThroughNarrowViews(t *testing.T) {
	t.Parallel()
	dbtest.CheckStoreViews(t, "store.go", "privacy.go")
}
