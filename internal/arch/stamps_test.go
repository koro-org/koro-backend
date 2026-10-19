package arch_test

import (
	"fmt"
	"go/ast"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// One clock for the [ts] columns (02 §0.1, BE-064, BE-069): created_at and
// updated_at of a [ts] table come from the database alone, the column
// defaults (now()) on INSERT and koro_touch_updated_at() on UPDATE. A
// writable GORM field lets Create stamp the row with the application's
// clock instead (GORM fills a zero CreatedAt/UpdatedAt with its NowFunc,
// a caller's value is written as is), and a raw INSERT that names the
// columns does the same; the first UPDATE then writes the database clock,
// and a skew between the two hosts yields updated_at < created_at on one
// row.

var (
	// touchTrigger finds the [ts] tables of a migration: a row trigger
	// BEFORE UPDATE that runs koro_touch_updated_at() (an optional WHEN
	// clause in between, posts).
	touchTrigger = regexp.MustCompile(`(?is)CREATE\s+TRIGGER\s+\w+\s+BEFORE\s+UPDATE\s+ON\s+([\w.]+)\s+FOR\s+EACH\s+ROW\s+` +
		`(?:WHEN\s*\([^;]*?\)\s*)?EXECUTE\s+(?:FUNCTION|PROCEDURE)\s+(?:public\.)?koro_touch_updated_at\s*\(\s*\)`)
	// rawInsert is the column list of an INSERT statement in a Go string.
	rawInsert = regexp.MustCompile(`(?is)INSERT\s+INTO\s+([\w.]+)\s*\(([^)]*)\)`)
	stampCol  = regexp.MustCompile(`(?i)\b(created_at|updated_at)\b`)
)

// writableStamps are the deliberate exceptions, "<file> <table>" of a raw
// INSERT that names a [ts] column, with the reason. A model field never is
// one: a business time lives in its own column.
var writableStamps = map[string]string{
	// The request's created_at and updated_at are its received_at (02 §22.1,
	// BE-049): a request taken in by e-mail or by post is entered later with
	// the time it reached Koro, and the 30-day answer deadline (due_at)
	// counts from there. Business time, set by the DPO's service; kept.
	"internal/kvkk/store_gorm.go data_subject_requests": "KVKK request received_at (business time)",
}

// tsTables reads the [ts] tables of root/db/migrations/*.up.sql.
func tsTables(t *testing.T, root string) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "db", "migrations", "*.up.sql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no migrations under %s: %v", root, err)
	}
	out := map[string]bool{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range touchTrigger.FindAllStringSubmatch(string(b), -1) {
			out[strings.TrimPrefix(strings.ToLower(m[1]), "public.")] = true
		}
	}
	return out
}

// gormModel is a struct type of a non-test file that maps created_at or
// updated_at; tagged: at least one field has a gorm tag.
type gormModel struct {
	file, name string
	tagged     bool
	stamps     []stampField // fields mapped to created_at or updated_at
	columns    map[string]bool
}

type stampField struct {
	name, column, tag string
}

// stampColumn is the column of a field: its gorm column tag or, without
// one, GORM's naming of CreatedAt/UpdatedAt; "" for any other field.
func stampColumn(name, tag string) string {
	for part := range strings.SplitSeq(tag, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), ":"); ok && strings.EqualFold(strings.TrimSpace(k), "column") {
			return strings.TrimSpace(v)
		}
	}
	switch name {
	case "CreatedAt":
		return "created_at"
	case "UpdatedAt":
		return "updated_at"
	}
	return ""
}

// writableTag reports whether GORM writes the field on INSERT or UPDATE
// (gorm.io/gorm/schema: "->" is read only unless "<-" gives the write back;
// "<-:create", "<-:update" write; "-" ignores the field).
func writableTag(tag string) bool {
	set := map[string]string{}
	for part := range strings.SplitSeq(tag, ";") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), ":")
		set[strings.ToUpper(strings.TrimSpace(k))] = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := set["-"]; ok && (v == "" || v == "all") {
		return false
	}
	if v, ok := set["<-"]; ok {
		return v == "" || strings.Contains(v, "create") || strings.Contains(v, "update")
	}
	_, readOnly := set["->"]
	return !readOnly
}

// gormModels returns the GORM models of files and the table each one names
// through TableName (a string literal; keyed "<dir>.<type>").
func gormModels(files []goFile) ([]gormModel, map[string]string) {
	var models []gormModel
	tables := map[string]string{}
	for _, f := range files {
		if f.test {
			continue
		}
		dir := path.Dir(f.rel)
		for _, d := range f.ast.Decls {
			switch x := d.(type) {
			case *ast.GenDecl:
				for _, s := range x.Specs {
					ts, ok := s.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					m := gormModel{file: f.rel, name: ts.Name.Name, columns: map[string]bool{}}
					for _, fl := range st.Fields.List {
						var tag string
						if fl.Tag != nil {
							raw, _ := strconv.Unquote(fl.Tag.Value)
							tag, m.tagged = reflect.StructTag(raw).Get("gorm"), m.tagged || strings.Contains(raw, `gorm:"`)
						}
						for _, n := range fl.Names {
							if col := stampColumn(n.Name, tag); col == "created_at" || col == "updated_at" {
								m.stamps, m.columns[col] = append(m.stamps, stampField{name: n.Name, column: col, tag: tag}), true
							}
						}
					}
					if len(m.stamps) > 0 {
						models = append(models, m)
					}
				}
			case *ast.FuncDecl:
				if x.Name.Name != "TableName" || x.Recv == nil || len(x.Recv.List) != 1 || x.Body == nil || len(x.Body.List) != 1 {
					continue
				}
				recv := x.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				id, ok := recv.(*ast.Ident)
				ret, ok2 := x.Body.List[0].(*ast.ReturnStmt)
				if ok && ok2 && len(ret.Results) == 1 {
					tables[dir+"."+id.Name] = literal(ret.Results[0])
				}
			}
		}
	}
	return models, tables
}

// stampViolations is the one-clock rule: a GORM model of a [ts] table maps
// created_at and updated_at read only (`gorm:"column:created_at;->"`), and
// no raw INSERT into a [ts] table names them. A model without TableName
// that maps both columns with gorm tags (a row type of Table() or Raw)
// counts as a [ts] model too. Exceptions: writableStamps (raw INSERTs
// only).
func stampViolations(files []goFile, ts map[string]bool, allow map[string]string) (v, used []string) {
	models, tables := gormModels(files)
	for _, m := range models {
		table, named := tables[path.Dir(m.file)+"."+m.name]
		switch {
		case named && !ts[table]:
			continue
		case !named && (!m.tagged || !m.columns["created_at"] || !m.columns["updated_at"]):
			continue
		case !named:
			table = "(a [ts] row type)"
		}
		for _, s := range m.stamps {
			if writableTag(s.tag) {
				v = append(v, fmt.Sprintf("%s: %s.%s maps %s of %s writable: tag it `gorm:\"column:%s;->\"`, the database stamps it (02 §0.1, BE-069)",
					m.file, m.name, s.name, s.column, table, s.column))
			}
		}
	}
	for _, f := range files {
		if f.test {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			var sql string
			switch x := n.(type) {
			case *ast.BasicLit:
				sql = literal(x)
			case *ast.BinaryExpr: // "INSERT INTO t (" + cols + ") …": the literal parts at once
				if sql = literal(x); sql == "" {
					return true
				}
			default:
				return true
			}
			for _, m := range rawInsert.FindAllStringSubmatch(sql, -1) {
				table := strings.TrimPrefix(strings.ToLower(m[1]), "public.")
				cols := stampCol.FindAllString(m[2], -1)
				if !ts[table] || len(cols) == 0 {
					continue
				}
				key := f.rel + " " + table
				if _, ok := allow[key]; ok {
					used = append(used, key)
					continue
				}
				v = append(v, fmt.Sprintf("%s: INSERT INTO %s names %s: leave it to the column default (02 §0.1, BE-069)",
					f.rel, table, strings.ToLower(strings.Join(cols, ", "))))
			}
			return false
		})
	}
	return v, used
}

func TestArch_TsStampsFromDatabase(t *testing.T) {
	t.Parallel()
	tables := tsTables(t, repoRoot)
	for _, must := range []string{"payments", "orders", "users", "files", "posts", "talent.cohorts"} {
		if !tables[must] {
			t.Fatalf("the %d [ts] tables lack %q: the trigger pattern no longer matches the migrations", len(tables), must)
		}
	}
	v, used := stampViolations(parseTree(t, repoRoot), tables, writableStamps)
	if len(v) > 0 {
		t.Errorf("one clock for [ts] columns:\n%s", strings.Join(v, "\n"))
	}
	for key := range writableStamps {
		if !slices.Contains(used, key) {
			t.Errorf("writableStamps entry %q matches no INSERT: remove it", key)
		}
	}
	got, _ := stampViolations(parseTree(t, violationsRoot), tsTables(t, violationsRoot), writableStamps)
	want := []string{
		"internal/events/stamps.go: Event.CreatedAt maps created_at of events writable: tag it `gorm:\"column:created_at;->\"`, the database stamps it (02 §0.1, BE-069)",
		"internal/events/stamps.go: Event.UpdatedAt maps updated_at of events writable: tag it `gorm:\"column:updated_at;->\"`, the database stamps it (02 §0.1, BE-069)",
		"internal/events/stamps.go: Form.CreatedAt maps created_at of event_forms writable: tag it `gorm:\"column:created_at;->\"`, the database stamps it (02 §0.1, BE-069)",
		"internal/events/stamps.go: INSERT INTO event_forms names updated_at: leave it to the column default (02 §0.1, BE-069)",
		"internal/events/stamps.go: INSERT INTO posts names created_at, updated_at: leave it to the column default (02 §0.1, BE-069)",
		"internal/events/stamps.go: Upsert.UpdatedAt maps updated_at of event_forms writable: tag it `gorm:\"column:updated_at;->\"`, the database stamps it (02 §0.1, BE-069)",
		"internal/events/stamps.go: row.CreatedAt maps created_at of (a [ts] row type) writable: tag it `gorm:\"column:created_at;->\"`, the database stamps it (02 §0.1, BE-069)",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("rule on %s:\n got  %q\n want %q", violationsRoot, got, want)
	}
}
