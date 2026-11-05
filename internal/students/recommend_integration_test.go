//go:build integration

package students_test

import (
	"testing"

	"koro/internal/platform/testutil"
	"koro/internal/students"
)

func TestRecommendStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	recommendContract(t, func(t *testing.T) (students.Store, world) {
		d := testutil.NewDB(t)
		return students.NewGormStore(d.ORM), pgWorld(d)
	})
}
