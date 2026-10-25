package db_test

import (
	"testing"
	"time"

	"koro/internal/platform/db"
)

func TestUTCPtr(t *testing.T) {
	t.Parallel()
	if db.UTCPtr(nil) != nil {
		t.Fatal("UTCPtr(nil) is not nil")
	}
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.FixedZone("TRT", 3*3600))
	got := db.UTCPtr(&at)
	if got == &at || got.Location() != time.UTC || !got.Equal(at) {
		t.Fatalf("UTCPtr = %v (%p vs %p)", got, got, &at)
	}
}

func TestNullable(t *testing.T) {
	t.Parallel()
	if db.Nullable("") != nil {
		t.Fatal(`Nullable("") is not nil`)
	}
	if p := db.Nullable("x"); p == nil || *p != "x" {
		t.Fatalf(`Nullable("x") = %v`, p)
	}
}

func TestLockIf_ForUpdateOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	orm := dryRun(t)
	for lock, want := range map[bool]string{
		true:  `SELECT * FROM "events" WHERE id = $1 LIMIT $2 FOR UPDATE`,
		false: `SELECT * FROM "events" WHERE id = $1 LIMIT $2`,
	} {
		var e event
		stmt := db.LockIf(orm.Where("id = ?", 1), lock).Take(&e).Statement
		if got := stmt.SQL.String(); got != want {
			t.Errorf("LockIf(%v) SQL = %s, want %s", lock, got, want)
		}
	}
}
