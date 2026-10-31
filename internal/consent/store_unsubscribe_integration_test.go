//go:build integration

package consent_test

import (
	"testing"
	"uuid"

	"koro/internal/consent"
	"koro/internal/platform/testutil"
)

func TestUnsubscribeLinkStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	unsubscribeLinkContract(t, func(t *testing.T) storeWorld {
		d := testutil.NewDB(t)
		return storeWorld{s: consent.NewGormStore(d.ORM), tx: d.Tx, user: func(t *testing.T) uuid.UUID { return insertUser(t, d, uuid.NewV7()) }}
	})
}
