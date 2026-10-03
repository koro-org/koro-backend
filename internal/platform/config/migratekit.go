package config

import "fmt"

// Migratekit configures `koro migrate` (internal/platform/migratekit; 05 §7,
// §12.1). Owner: BE-002. MIGRATE_DATABASE_URL uses the owner role
// (app_owner) and connects to PostgreSQL directly, never through PgBouncer:
// golang-migrate holds a session-level advisory lock.
type Migratekit struct {
	URL Secret `env:"MIGRATE_DATABASE_URL,required,notEmpty"`
}

func (m *Migratekit) validate(Common) error {
	if _, err := postgresURLUser(m.URL.Reveal()); err != nil {
		return fmt.Errorf("MIGRATE_DATABASE_URL %w", err)
	}
	return nil
}
