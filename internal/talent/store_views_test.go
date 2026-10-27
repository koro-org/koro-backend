package talent_test

import (
	"testing"

	"koro/internal/platform/db/dbtest"
)

// TestStore_ReachedThroughNarrowViews keeps the BE-146 split (05 §2.2): a
// service file calls the store only through the view of its aggregate
// (s.roles(), s.configs(), s.dailyCounters(), s.ledger(),
// s.neutralizations(), s.scores(), s.scoring(), s.reconciliation();
// store.go), the retention sweeper through the views it holds
// (retention.go: the purges and talentTx). Only the accessors themselves
// and the KVKK provider (privacy.go: export and erasure cover every table)
// hold the whole Store.
func TestStore_ReachedThroughNarrowViews(t *testing.T) {
	t.Parallel()
	dbtest.CheckStoreViews(t, "store.go", "privacy.go")
}
