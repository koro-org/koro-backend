// Package migrations embeds the SQL migrations of Koro (05 §7). Files are
// NNNNNN_<snake_case>.{up,down}.sql in the order of 02 §23; numbers are
// allocated per ticket (BE-001 R2). Run them only with `koro migrate`
// (internal/platform/migratekit); the application never migrates at start.
package migrations

import "embed"

// FS holds every *.sql migration file (db/migrations/.frozen is read from
// disk by the lint test, it is not embedded).
//
//go:embed *.sql
var FS embed.FS
