//go:build integration

package events_test

import (
	"testing"

	"koro/internal/events"
	"koro/internal/platform/testutil"
)

func TestStore_ParticipationContract_Gorm(t *testing.T) {
	t.Parallel()
	participationContract(t, func(t *testing.T) (events.Store, world) {
		d := testutil.NewDB(t)
		return events.NewGormStore(d.ORM), pgWorld(d)
	})
}

func TestStatsStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	statsContract(t, func(t *testing.T) (events.Store, world) {
		d := testutil.NewDB(t)
		return events.NewGormStore(d.ORM), pgWorld(d)
	})
}
