package db

import (
	"errors"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrMissingOrgScope is the programmer error of a store call without an org
// (05 §5.5). The query is not executed: a nil org never scans a whole table.
var ErrMissingOrgScope = errors.New("db: org scope required")

// ScopeOrg restricts a query on an org-owned table to orgID:
//
//	db.Conn(ctx, s.db.ORM).Scopes(db.ScopeOrg(orgID)).Where("id = ?", id).Take(&e)
//
// The column is qualified with the statement's table, so joins stay
// unambiguous. Every method of a store on an org-owned table applies it
// (reads, updates, deletes, counts); raw SQL writes "org_id = @org_id".
// Methods on tables without an org carry a "// cross-org: <reason>" doc
// comment instead (internal/arch checks both).
func ScopeOrg(orgID uuid.UUID) func(*gorm.DB) *gorm.DB {
	return func(tx *gorm.DB) *gorm.DB {
		if orgID == uuid.Nil() {
			_ = tx.AddError(ErrMissingOrgScope)
			return tx
		}
		return tx.Where(clause.Eq{Column: clause.Column{Table: clause.CurrentTable, Name: "org_id"}, Value: orgID})
	}
}
