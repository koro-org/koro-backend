//go:build integration

package inbox_test

import (
	"os"
	"testing"

	"koro/internal/platform/db"
	"koro/internal/platform/inbox"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// Every backend test also runs against PostgreSQL (app_rw, Exec mode).
var _ = addBackend(backend{"postgres", func(t *testing.T) (inbox.Store, db.TxManager, func(t *testing.T) []inbox.Row) {
	d := testutil.NewDB(t)
	return inbox.NewGormStore(d.ORM), d.Tx, func(t *testing.T) []inbox.Row {
		t.Helper()
		var rows []inbox.Row
		if err := d.ORM.WithContext(t.Context()).Order("received_at, id").Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		return rows
	}
}})
