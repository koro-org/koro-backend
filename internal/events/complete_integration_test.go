//go:build integration

package events_test

import (
	"testing"

	"koro/internal/events"
	"koro/internal/platform/testutil"
)

func TestStore_CompleteContract_Gorm(t *testing.T) {
	t.Parallel()
	completeContract(t, func(t *testing.T) (events.Store, world) {
		d := testutil.NewDB(t)
		return events.NewGormStore(d.ORM), pgWorld(d)
	})
}
