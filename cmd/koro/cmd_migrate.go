package main

import (
	"context"
	"fmt"
)

// runMigrate runs `koro migrate up | down N | version | force V` with the
// owner role (MIGRATE_DATABASE_URL; 05 §7). Filled by BE-002.
func runMigrate(_ context.Context, _ []string) error {
	return fmt.Errorf("%w (BE-002)", errNotImplemented)
}
