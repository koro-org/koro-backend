package db

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"koro/internal/platform/problem"
)

// SQLSTATE codes interpreted by this package (PostgreSQL Appendix A).
const (
	CodeUniqueViolation      = "23505"
	CodeForeignKeyViolation  = "23503"
	CodeCheckViolation       = "23514"
	CodeExclusionViolation   = "23P01"
	CodeRestrictViolation    = "23001" // koro_forbid_mutation, koro_policy_close_only, audit_events_guard
	CodeSerializationFailure = "40001"
	CodeDeadlockDetected     = "40P01"
	CodeInsufficientPriv     = "42501"
)

// IsNotFound reports a missing row (GORM Take/First, pgx, database/sql).
func IsNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows)
}

// IsUnique reports a unique violation (23505).
func IsUnique(err error) bool { return SQLState(err) == CodeUniqueViolation }

// IsForeignKey reports a foreign key violation (23503).
func IsForeignKey(err error) bool { return SQLState(err) == CodeForeignKeyViolation }

// IsCheck reports a CHECK violation (23514).
func IsCheck(err error) bool { return SQLState(err) == CodeCheckViolation }

// IsRetryable reports a serialization failure or a deadlock (40001, 40P01):
// the whole transaction may be retried (WithSerializableTx does).
func IsRetryable(err error) bool {
	c := SQLState(err)
	return c == CodeSerializationFailure || c == CodeDeadlockDetected
}

// Translate maps a database error onto the generic problem sentinels
// (05 §6.6); the original error stays wrapped for logs and for
// ConstraintName/SQLState:
//
//	missing row                          → problem.ErrNotFound   (404)
//	unique, exclusion, foreign key       → problem.ErrConflict   (409)
//	check                                → problem.ErrValidation (422)
//	serialization failure, deadlock      → problem.ErrConflict   (409, retry)
//
// Everything else (lock/statement timeout, connection errors, NOT NULL,
// immutability triggers 23001, ErrMissingOrgScope) is returned unchanged
// and becomes a 500 without details. An error that already is a
// *problem.Error is returned as is. Deferred constraints surface at COMMIT
// and go through the same mapping (WithTx); a constraint trigger reports
// itself with RAISE ... USING ERRCODE = 'check_violation' (or
// 'unique_violation'), CONSTRAINT = '<name>' so that modules can map it.
// Modules map their own constraint names first (Constraints).
func Translate(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*problem.Error](err); ok {
		return err
	}
	if IsNotFound(err) {
		return problem.ErrNotFound.Wrap(err)
	}
	switch SQLState(err) {
	case CodeUniqueViolation, CodeExclusionViolation, CodeForeignKeyViolation,
		CodeSerializationFailure, CodeDeadlockDetected:
		return problem.ErrConflict.Wrap(err)
	case CodeCheckViolation:
		return problem.ErrValidation.Wrap(err)
	}
	return err
}

// Constraints maps explicit constraint names (02 §23 naming) to module
// sentinels. A store declares one table and translates with it:
//
//	var constraints = db.Constraints{"events_org_slug_uq": ErrSlugTaken}
//	return constraints.Translate(err)
//
// Unlisted constraints fall back to Translate.
type Constraints map[string]*problem.Error

// Translate returns the sentinel of the violated constraint, wrapping the
// PostgreSQL error, or Translate(err).
func (c Constraints) Translate(err error) error {
	if name := ConstraintName(err); name != "" {
		if p, ok := c[name]; ok {
			pe, _ := errors.AsType[*pgconn.PgError](err)
			return p.Wrap(pe)
		}
	}
	return Translate(err)
}
