//go:build integration

package events_test

import (
	"testing"

	"koro/internal/events"
	"koro/internal/platform/testutil"
)

// TestStore_MineContract_Gorm runs BE-095's store contract on PostgreSQL.
func TestStore_MineContract_Gorm(t *testing.T) {
	t.Parallel()
	mineStoreContract(t, func(t *testing.T) (events.Store, world) {
		d := testutil.NewDB(t)
		return events.NewGormStore(d.ORM), pgWorld(d)
	})
}
