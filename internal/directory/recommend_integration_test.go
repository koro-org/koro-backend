//go:build integration

package directory_test

import (
	"testing"

	"koro/internal/directory"
	"koro/internal/orgs"
	"koro/internal/platform/testutil"
)

func TestRecommendStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	recommendContract(t, func(t *testing.T) (directory.Store, world) {
		d := testutil.NewDB(t)
		return directory.NewGormStore(d.ORM), orgWorld(orgs.NewGormStore(d.ORM), pgUser(d))
	})
}
