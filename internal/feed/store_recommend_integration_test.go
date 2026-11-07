//go:build integration

package feed_test

import (
	"testing"

	"koro/internal/feed"
	"koro/internal/platform/testutil"
)

func TestStore_RecommendContract_Gorm(t *testing.T) {
	t.Parallel()
	recommendContract(t, func(t *testing.T) (feed.Store, world) {
		d := testutil.NewDB(t)
		return feed.NewGormStore(d.ORM), pgWorld(d)
	})
}
