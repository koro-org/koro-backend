//go:build integration

package testutil_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// TestNewDB_IsolatedInParallel: parallel tests each get their own migrated
// database; a row written in one is invisible in the others.
func TestNewDB_IsolatedInParallel(t *testing.T) {
	t.Parallel()
	for i := range 8 {
		t.Run(fmt.Sprintf("db%d", i), func(t *testing.T) {
			t.Parallel()
			d := testutil.NewDB(t)
			ctx := t.Context()
			var user, dbName string
			if err := d.Pool.QueryRow(ctx, `SELECT current_user, current_database()`).Scan(&user, &dbName); err != nil {
				t.Fatal(err)
			}
			if user != "app_rw" {
				t.Fatalf("connected as %q, want app_rw", user)
			}
			var before int
			if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if before != 0 {
				t.Fatalf("fresh database %s already has %d outbox rows", dbName, before)
			}
			for range i + 1 {
				if _, err := d.Pool.Exec(ctx, `INSERT INTO outbox (topic, aggregate_type, aggregate_id, payload)
					VALUES ('test.created', 'test', uuidv7(), '{}')`); err != nil {
					t.Fatal(err)
				}
			}
			var after int
			if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != i+1 {
				t.Fatalf("database %s has %d outbox rows, want %d (not isolated)", dbName, after, i+1)
			}
			var version int
			if err := d.Pool.QueryRow(ctx, `SELECT version FROM schema_migrations`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if uint(version) != testutil.LatestMigration(t) {
				t.Fatalf("schema version %d, want %d", version, testutil.LatestMigration(t))
			}
		})
	}
}

func TestNewRedis_EmptyAndIsolated(t *testing.T) {
	t.Parallel()
	a, b := testutil.NewRedis(t), testutil.NewRedis(t)
	ctx := t.Context()
	if a.Options().DB == b.Options().DB || a.Options().DB == 0 {
		t.Fatalf("redis databases %d and %d must differ and not be 0", a.Options().DB, b.Options().DB)
	}
	if err := a.Set(ctx, "k", "a", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if n, err := b.Exists(ctx, "k").Result(); err != nil || n != 0 {
		t.Fatalf("key leaked between databases: %d, %v", n, err)
	}
	policy, err := a.ConfigGet(ctx, "maxmemory-policy").Result()
	if err != nil || policy["maxmemory-policy"] != "noeviction" {
		t.Fatalf("maxmemory-policy = %v, %v; want noeviction", policy, err)
	}
}

func TestQueryCounter_CountsMarkedStatements(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	qc := testutil.NewQueryCounter()
	ctx := qc.Context(t.Context())

	var n int64
	if err := db.Conn(ctx, d.ORM).Table("outbox").Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := db.Conn(ctx, d.ORM).Table("outbox").Limit(5).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Conn(ctx, d.ORM).Exec(`SELECT 1`).Error; err != nil {
		t.Fatal(err)
	}
	// Unmarked context: not counted.
	if err := db.Conn(t.Context(), d.ORM).Exec(`SELECT 1`).Error; err != nil {
		t.Fatal(err)
	}
	err := d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return db.Conn(ctx, d.ORM).Exec(`SELECT 1`).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := qc.Count(); got != 4 {
		t.Fatalf("Count() = %d, want 4", got)
	}
	qc.Reset()
	if qc.Count() != 0 {
		t.Fatal("Reset() did not zero the counter")
	}
}
