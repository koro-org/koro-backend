package migrations_test

import (
	"errors"
	"io/fs"
	"os"
	"testing"

	"koro/db/migrations"
	"koro/internal/platform/migratekit"
)

// TestMigrations_Lint runs the migration rules (05 §7, BE-001 R2) on the real
// directory, including .frozen once the pilot froze a floor.
func TestMigrations_Lint(t *testing.T) {
	t.Parallel()
	if errs := migratekit.Lint(os.DirFS(".")); len(errs) > 0 {
		t.Fatal(errors.Join(errs...))
	}
}

// TestMigrations_EmbedHasEveryFile guards the //go:embed pattern: the binary
// must carry exactly the *.sql files of the directory.
func TestMigrations_EmbedHasEveryFile(t *testing.T) {
	t.Parallel()
	onDisk, err := fs.Glob(os.DirFS("."), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk) == 0 || len(onDisk) != len(embedded) {
		t.Fatalf("embedded %d files, directory has %d", len(embedded), len(onDisk))
	}
}
