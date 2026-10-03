package migratekit_test

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"koro/internal/platform/migratekit"
)

const (
	okUp   = "BEGIN;\nSET LOCAL lock_timeout = '5s';\nCREATE TABLE t (id uuid PRIMARY KEY);\nCOMMIT;\n"
	okDown = "BEGIN;\nDROP TABLE IF EXISTS t;\nCOMMIT;\n"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

// validFS returns three complete, valid migration pairs.
func validFS() fstest.MapFS {
	return fstest.MapFS{
		"000001_create_t.up.sql":     file(okUp),
		"000001_create_t.down.sql":   file(okDown),
		"000002_create_fn.up.sql":    file("-- one statement needs no BEGIN\nCREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1; $$;\n"),
		"000002_create_fn.down.sql":  file("DROP FUNCTION IF EXISTS f();\n"),
		"000003_idx_t_conc.up.sql":   file("CREATE INDEX CONCURRENTLY IF NOT EXISTS t_x_idx ON t (id);\n"),
		"000003_idx_t_conc.down.sql": file("DROP INDEX CONCURRENTLY IF EXISTS t_x_idx;\n"),
		"embed.go":                   file("package migrations\n"),
	}
}

func lintMessages(fsys fstest.MapFS) string {
	var sb strings.Builder
	for _, err := range migratekit.Lint(fsys) {
		sb.WriteString(err.Error())
		sb.WriteString("\n")
	}
	return sb.String()
}

func TestMigrationLint_PairsAndNaming(t *testing.T) {
	t.Parallel()
	if errs := migratekit.Lint(validFS()); len(errs) > 0 {
		t.Fatalf("valid migrations rejected: %v", errors.Join(errs...))
	}

	tests := []struct {
		name   string
		mutate func(fstest.MapFS)
		want   string
	}{
		{"missing down", func(f fstest.MapFS) { delete(f, "000001_create_t.down.sql") }, "000001_create_t: up/down pair incomplete"},
		{"missing up", func(f fstest.MapFS) { delete(f, "000002_create_fn.up.sql") }, "000002_create_fn: up/down pair incomplete"},
		{"bad name", func(f fstest.MapFS) { f["0004_Create-T.up.sql"] = file(okUp) }, "0004_Create-T.up.sql: invalid name"},
		{"stray file", func(f fstest.MapFS) { f["notes.txt"] = file("x") }, "notes.txt: invalid name"},
		{"zero number", func(f fstest.MapFS) { f["000000_x.up.sql"] = file(okUp) }, "numbers start at 000001"},
		{"duplicate number", func(f fstest.MapFS) {
			f["000002_other.up.sql"] = file(okUp)
			f["000002_other.down.sql"] = file(okDown)
		}, "000002: number used by two migrations"},
		{"multi statement without BEGIN", func(f fstest.MapFS) {
			f["000001_create_t.up.sql"] = file("CREATE TABLE t (id int);\nCREATE INDEX t_idx ON t (id);\n")
		}, "000001_create_t.up.sql: a multi-statement migration must be wrapped in BEGIN … COMMIT"},
		{"BEGIN without COMMIT", func(f fstest.MapFS) {
			f["000001_create_t.up.sql"] = file("BEGIN;\nCREATE TABLE t (id int);\n")
		}, "must be wrapped in BEGIN … COMMIT"},
		{"CONCURRENTLY with other statements", func(f fstest.MapFS) {
			f["000003_idx_t_conc.up.sql"] = file("CREATE TABLE u (id int);\nCREATE INDEX CONCURRENTLY u_idx ON u (id);\n")
		}, "000003_idx_t_conc.up.sql: a CONCURRENTLY migration must be a single statement without BEGIN"},
		{"CONCURRENTLY inside BEGIN", func(f fstest.MapFS) {
			f["000003_idx_t_conc.up.sql"] = file("BEGIN;\nCREATE INDEX CONCURRENTLY u_idx ON u (id);\nCOMMIT;\n")
		}, "a CONCURRENTLY migration must be a single statement"},
		{"DROP TABLE in up", func(f fstest.MapFS) {
			f["000001_create_t.up.sql"] = file("BEGIN;\nDROP TABLE old_t;\nCOMMIT;\n")
		}, "000001_create_t.up.sql: destructive statement needs '-- koro:allow-destructive"},
		{"DROP COLUMN in up", func(f fstest.MapFS) {
			f["000001_create_t.up.sql"] = file("ALTER TABLE t DROP COLUMN x;\n")
		}, "destructive statement"},
		{"TRUNCATE in up", func(f fstest.MapFS) {
			f["000001_create_t.up.sql"] = file("TRUNCATE t;\n")
		}, "destructive statement"},
		{"DELETE in a DO block", func(f fstest.MapFS) {
			f["000001_create_t.up.sql"] = file("DO $$ BEGIN DELETE FROM t; END $$;\n")
		}, "destructive statement"},
		{"RENAME COLUMN in up", func(f fstest.MapFS) {
			f["000001_create_t.up.sql"] = file("ALTER TABLE t RENAME COLUMN a TO b;\n")
		}, "destructive statement"},
		{"unterminated string", func(f fstest.MapFS) {
			f["000001_create_t.up.sql"] = file("SELECT 'x;\n")
		}, "unterminated string literal"},
		{"unterminated body", func(f fstest.MapFS) {
			f["000002_create_fn.up.sql"] = file("CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $body$ SELECT 1;\n")
		}, "unterminated dollar-quoted body $body$"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := validFS()
			tt.mutate(f)
			if got := lintMessages(f); !strings.Contains(got, tt.want) {
				t.Fatalf("Lint() = %q, want an error containing %q", got, tt.want)
			}
		})
	}

	// Allowed: annotated destructive up, destructive down, destructive words
	// in comments/strings/function bodies, TRUNCATE triggers, gaps.
	allowed := validFS()
	allowed["000001_create_t.up.sql"] = file("-- koro:allow-destructive replaced by t2 (BE-999)\nBEGIN;\nDROP TABLE old_t;\nCOMMIT;\n")
	allowed["000002_create_fn.up.sql"] = file(`BEGIN;
-- DROP TABLE x; is only a comment /* DELETE FROM y; */
CREATE FUNCTION purge() RETURNS void LANGUAGE plpgsql AS $fn$ BEGIN DELETE FROM t; END $fn$;
CREATE TRIGGER t_guard BEFORE TRUNCATE ON t FOR EACH STATEMENT EXECUTE FUNCTION g();
COMMENT ON TABLE t IS 'never DROP TABLE t; it''s used';
SELECT E'it\'s; fine', "weird;name", $1;
COMMIT;
`)
	allowed["000009_create_far.up.sql"] = file(okUp)
	allowed["000009_create_far.down.sql"] = file(okDown)
	if errs := migratekit.Lint(allowed); len(errs) > 0 {
		t.Fatalf("allowed migrations rejected: %v", errors.Join(errs...))
	}
}

func TestMigrationLint_FrozenFloorRejectsEdit(t *testing.T) {
	t.Parallel()
	base := validFS()
	manifest, err := migratekit.Freeze(base, 2)
	if err != nil {
		t.Fatalf("Freeze() error = %v", err)
	}
	if !strings.Contains(string(manifest), "floor 000002") || strings.Contains(string(manifest), "000003_") {
		t.Fatalf("manifest must freeze exactly 000001..000002:\n%s", manifest)
	}
	frozen := func() fstest.MapFS {
		f := validFS()
		f[migratekit.FrozenFile] = file(string(manifest))
		return f
	}
	if errs := migratekit.Lint(frozen()); len(errs) > 0 {
		t.Fatalf("unchanged frozen migrations rejected: %v", errors.Join(errs...))
	}

	tests := []struct {
		name   string
		mutate func(fstest.MapFS)
		want   string
	}{
		{"edit below floor", func(f fstest.MapFS) {
			f["000001_create_t.up.sql"] = file(okUp + "-- harmless looking edit\n")
		}, "000001_create_t.up.sql: changed after it was frozen (floor 000002)"},
		{"edit at floor", func(f fstest.MapFS) {
			f["000002_create_fn.down.sql"] = file("DROP FUNCTION f();\n")
		}, "000002_create_fn.down.sql: changed after it was frozen"},
		{"new file below floor", func(f fstest.MapFS) {
			f["000002_sneaky.up.sql"] = file(okUp)
			f["000002_sneaky.down.sql"] = file(okDown)
		}, "000002_sneaky.up.sql: added at or below the frozen floor 000002"},
		{"frozen file removed", func(f fstest.MapFS) {
			delete(f, "000001_create_t.down.sql")
		}, "000001_create_t.down.sql: frozen migration was removed or renamed"},
		{"floor only", func(f fstest.MapFS) {
			f[migratekit.FrozenFile] = file("floor 2\n")
		}, "no checksums: regenerate it with `koro migrate freeze 2`"},
		{"garbage", func(f fstest.MapFS) {
			f[migratekit.FrozenFile] = file("floor 2\nnot a checksum line\n")
		}, ".frozen line 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := frozen()
			tt.mutate(f)
			if got := lintMessages(f); !strings.Contains(got, tt.want) {
				t.Fatalf("Lint() = %q, want an error containing %q", got, tt.want)
			}
		})
	}

	// Above the floor everything stays open: edit 000003, add 000004.
	open := frozen()
	open["000003_idx_t_conc.down.sql"] = file("DROP INDEX CONCURRENTLY IF EXISTS t_x_idx; -- edited\n")
	open["000004_create_u.up.sql"] = file(okUp)
	open["000004_create_u.down.sql"] = file(okDown)
	if errs := migratekit.Lint(open); len(errs) > 0 {
		t.Fatalf("changes above the floor rejected: %v", errors.Join(errs...))
	}

	for _, floor := range []uint{0, 4} {
		if _, err := migratekit.Freeze(base, floor); err == nil {
			t.Errorf("Freeze(floor=%d) succeeded outside 1..3", floor)
		}
	}
}

func TestLatest(t *testing.T) {
	t.Parallel()
	got, err := migratekit.Latest(validFS())
	if err != nil || got != 3 {
		t.Fatalf("Latest() = %d, %v; want 3", got, err)
	}
	if got, err := migratekit.Latest(fstest.MapFS{}); err != nil || got != 0 {
		t.Fatalf("Latest(empty) = %d, %v; want 0", got, err)
	}
}
