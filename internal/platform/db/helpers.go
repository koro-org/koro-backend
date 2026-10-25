package db

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LockIf adds FOR UPDATE to q when lock is set (a read that a write in the
// same transaction depends on, 05 §5.4).
func LockIf(q *gorm.DB, lock bool) *gorm.DB {
	if lock {
		return q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	return q
}

// UTCPtr returns t in UTC (nil stays nil): timestamps leave the service and
// enter the database in UTC.
func UTCPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// Nullable maps "" to NULL for an optional text column.
func Nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
