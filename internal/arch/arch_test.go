package arch_test

import (
	_ "embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	repoRoot       = "../.."               // internal/arch → repo root
	violationsRoot = "testdata/violations" // miniature tree breaking every rule
	modPath        = "koro/internal/"      // go.mod module path + internal/
)

// layers is 01 §5.1 (L1–L13). platform/* and authz are L0 leaves; cmd is
// L14 (composition root). A new module is added to 01 first.
var layers = map[string]int{
	"audit": 1, "files": 2, "consent": 2, "entitlements": 2, "notify": 3, "identity": 4, "orgs": 5,
	"directory": 6, "students": 7, "parties": 7, "events": 8, "talent": 8, "outreach": 8, "feed": 9,
	"catalog": 9, "discovery": 9, "messaging": 10, "orders": 10, "ledger": 11, "billing": 11,
	"payments": 12, "kvkk": 13,
}

// talentImporters may import talent (01 §5.4); cmd/ may too.
var talentImporters = map[string]bool{"talent": true, "discovery": true, "kvkk": true}

//go:embed sync_writes.txt
var syncWritesTxt string

// goFile is one parsed Go file of internal/ or cmd/.
type goFile struct {
	rel  string // slash path relative to the root ("internal/events/service.go")
	top  string // "events", "platform", "authz", ... or "cmd"
	test bool
	ast  *ast.File
}

// parseTree parses every .go file under root/internal and root/cmd,
// skipping testdata directories.
func parseTree(t *testing.T, root string) []goFile {
	t.Helper()
	var out []goFile
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && d.Name() == "testdata":
				return filepath.SkipDir
			case d.IsDir() || !strings.HasSuffix(p, ".go"):
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			top := "cmd"
			if dir == "internal" {
				top = strings.Split(rel, "/")[1]
			}
			out = append(out, goFile{rel: rel, top: top, test: strings.HasSuffix(p, "_test.go"), ast: f})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no Go files under %s", root)
	}
	return out
}

func imports(f goFile) []string {
	var out []string
	for _, imp := range f.ast.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		out = append(out, p)
	}
	return out
}

// layerViolations is 01 §5.1/§5.4: a feature module imports only platform,
// authz and lower layers; platform and authz import no feature module;
// authztest (test resolver, personas) is imported by tests only.
func layerViolations(files []goFile) []string {
	var v []string
	for _, f := range files {
		if f.test || f.top == "cmd" || f.top == "arch" {
			continue
		}
		srcLayer, feature := layers[f.top]
		if !feature && f.top != "platform" && f.top != "authz" {
			v = append(v, fmt.Sprintf("%s: unknown package internal/%s: add the module to 01 §5.1 first", f.rel, f.top))
			continue
		}
		for _, p := range imports(f) {
			sub, ok := strings.CutPrefix(p, modPath)
			if !ok {
				continue
			}
			dst := strings.Split(sub, "/")[0]
			dstLayer, dstFeature := layers[dst]
			switch {
			case p == modPath+"authz/authztest" || strings.HasPrefix(p, modPath+"authz/authztest/"):
				v = append(v, fmt.Sprintf("%s imports %s: authztest is for tests only", f.rel, p))
			case dst == "platform" || dst == "authz":
			case !dstFeature:
				v = append(v, fmt.Sprintf("%s imports %s: unknown package", f.rel, p))
			case !feature:
				v = append(v, fmt.Sprintf("%s imports %s: internal/%s is a leaf and imports no feature module (01 §5.4)", f.rel, p, f.top))
			case dstLayer >= srcLayer:
				v = append(v, fmt.Sprintf("%s imports %s: L%d may import only lower layers, %s is L%d (01 §5.1)", f.rel, p, srcLayer, dst, dstLayer))
			}
		}
	}
	return v
}

// talentViolations is 01 §5.4: talent is imported only by discovery, kvkk
// and cmd (test files included: the talent value never leaks into tests of
// other modules either).
func talentViolations(files []goFile) []string {
	var v []string
	for _, f := range files {
		if f.top == "cmd" || talentImporters[f.top] {
			continue
		}
		for _, p := range imports(f) {
			if p == modPath+"talent" || strings.HasPrefix(p, modPath+"talent/") {
				v = append(v, fmt.Sprintf("%s imports %s: only discovery, kvkk and cmd may (01 §5.4)", f.rel, p))
			}
		}
	}
	return v
}

// foreignStoreViolations is 05 §2.4-1: Store, NewGormStore and NewMemStore
// of another module are referenced only by cmd/koro.
func foreignStoreViolations(files []goFile) []string {
	forbidden := map[string]bool{"Store": true, "NewGormStore": true, "NewMemStore": true}
	var v []string
	for _, f := range files {
		if f.test || layers[f.top] == 0 {
			continue // tests, cmd, platform, authz, arch
		}
		foreign := map[string]string{} // local import name → path
		for _, imp := range f.ast.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			sub, ok := strings.CutPrefix(p, modPath)
			if dst := strings.Split(sub, "/")[0]; !ok || dst == f.top || layers[dst] == 0 {
				continue
			}
			name := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			foreign[name] = p
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && foreign[x.Name] != "" && forbidden[sel.Sel.Name] {
					v = append(v, fmt.Sprintf("%s: %s.%s: another module's store is off limits (05 §2.4-1)", f.rel, x.Name, sel.Sel.Name))
				}
			}
			return true
		})
	}
	return v
}

// scopeViolations is 05 §5.5: every method in store_gorm*.go uses ScopeOrg
// (or the scoped helper, or "org_id = @org_id" raw SQL) or documents
// "cross-org:".
func scopeViolations(files []goFile) []string {
	var v []string
	for _, f := range files {
		base := f.rel[strings.LastIndex(f.rel, "/")+1:]
		if f.test || layers[f.top] == 0 || !strings.HasPrefix(base, "store_gorm") {
			continue
		}
		for _, d := range f.ast.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Body == nil {
				continue
			}
			if fd.Doc != nil && strings.Contains(fd.Doc.Text(), "cross-org:") {
				continue
			}
			scoped := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.Ident:
					scoped = scoped || x.Name == "ScopeOrg" || x.Name == "scoped"
				case *ast.BasicLit:
					scoped = scoped || (x.Kind == token.STRING && strings.Contains(x.Value, "org_id = @org_id"))
				}
				return !scoped
			})
			if !scoped {
				v = append(v, fmt.Sprintf("%s: %s has no org scope (ScopeOrg, scoped or a 'cross-org:' doc comment; 05 §5.5)", f.rel, fd.Name.Name))
			}
		}
	}
	return v
}

var (
	writeVerb  = regexp.MustCompile(`^(Record|Consume|Release|Grant|Revoke|Reserve|Post|Apply|Mark|Activate|Attach|Log|Schedule|Create|Update|Delete|Set|Cancel|Confirm|Submit)`)
	syncLineRe = regexp.MustCompile(`^(\*|[a-z]+)\s*->\s*([a-z]+)\.([A-Z][A-Za-z0-9]*)\.([A-Z][A-Za-z0-9]*)$`)
)

// parseSyncWrites reads the allow-list: "<caller> -> <module>.<Interface>.<Method>".
func parseSyncWrites(txt string) (map[string]bool, []string) {
	allow, seen := map[string]bool{}, map[string]bool{}
	var v []string
	for i, ln := range strings.Split(txt, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		m := syncLineRe.FindStringSubmatch(ln)
		switch {
		case m == nil:
			v = append(v, fmt.Sprintf("sync_writes.txt:%d: malformed line %q", i+1, ln))
			continue
		case m[1] != "*" && layers[m[1]] == 0:
			v = append(v, fmt.Sprintf("sync_writes.txt:%d: unknown caller module %q", i+1, m[1]))
		case layers[m[2]] == 0:
			v = append(v, fmt.Sprintf("sync_writes.txt:%d: unknown provider module %q", i+1, m[2]))
		case m[1] == m[2]:
			v = append(v, fmt.Sprintf("sync_writes.txt:%d: %s writing to itself is not a cross-module write", i+1, m[1]))
		}
		key := m[1] + " " + m[2] + "." + m[3] + "." + m[4]
		if seen[key] {
			v = append(v, fmt.Sprintf("sync_writes.txt:%d: duplicate line %q", i+1, ln))
		}
		seen[key], allow[key] = true, true
	}
	return allow, v
}

// syncWriteViolations is 05 §2.4-3: in a consumer's ports.go, an interface
// method starting with a write verb carries "// sync-write: <target>" and
// the (caller, target) pair is in sync_writes.txt (01 §6.3).
func syncWriteViolations(files []goFile, allow map[string]bool) []string {
	var v []string
	for _, f := range files {
		if f.test || layers[f.top] == 0 || !strings.HasSuffix(f.rel, "/"+f.top+"/ports.go") {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			it, ok := n.(*ast.InterfaceType)
			if !ok {
				return true
			}
			for _, m := range it.Methods.List {
				if len(m.Names) == 0 {
					continue
				}
				name, target := m.Names[0].Name, ""
				if m.Doc != nil {
					for ln := range strings.SplitSeq(m.Doc.Text(), "\n") {
						if t, ok := strings.CutPrefix(strings.TrimSpace(ln), "sync-write:"); ok {
							target = strings.TrimSpace(t)
						}
					}
				}
				switch {
				case target != "" && !allow[f.top+" "+target] && !allow["* "+target]:
					v = append(v, fmt.Sprintf("%s: %s: synchronous cross-module write %q is not in the 01 §6.3 allow-list (sync_writes.txt)", f.rel, name, target))
				case target == "" && writeVerb.MatchString(name):
					v = append(v, fmt.Sprintf("%s: %s starts with a write verb: mark it '// sync-write: <module>.<Interface>.<Method>' or use an event (05 §2.4-3)", f.rel, name))
				}
			}
			return true
		})
	}
	return v
}

// gormCalls are the GORM methods whose string arguments are SQL with "?"
// or "@name" parameters.
var gormCalls = map[string]bool{
	"Raw": true, "Exec": true, "Where": true, "Or": true, "Not": true, "Having": true, "Joins": true,
	"Select": true, "Order": true, "Group": true, "Distinct": true, "Expr": true,
}

// parenParam matches a parameter written directly after "(".
var parenParam = regexp.MustCompile(`\([?@]`)

// gormParamViolations is 06 H-30 (T-113): GORM expands a parameter written
// directly after "(" element by element (meant for "IN (?)"); stdlib
// uuid.UUID is a [16]byte array, so "VALUES (?" or "(@id" binds sixteen
// bytes and fails at run time (json.RawMessage and []byte too). Raw SQL
// writes "( ?" / "( @id", lists "IN ?" (internal/platform/db).
func gormParamViolations(files []goFile) []string {
	var v []string
	for _, f := range files {
		if f.test {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !gormCalls[sel.Sel.Name] {
				return true
			}
			for _, arg := range call.Args {
				if sql := literal(arg); parenParam.MatchString(sql) {
					v = append(v, fmt.Sprintf("%s: %s: parameter directly after \"(\" (GORM expands it element-wise): write \"( ?\" or \"IN ?\" (06 H-30)",
						f.rel, sel.Sel.Name))
					break
				}
			}
			return true
		})
	}
	return v
}

// literal returns the value of a string literal or a concatenation of
// string literals ("" otherwise).
func literal(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			s, err := strconv.Unquote(x.Value)
			if err == nil {
				return s
			}
		}
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return literal(x.X) + literal(x.Y)
		}
	case *ast.ParenExpr:
		return literal(x.X)
	}
	return ""
}

// check runs one rule on the real tree (no violation allowed) and on the
// violations tree (exactly the expected ones).
func check(t *testing.T, rule func([]goFile) []string, want ...string) {
	t.Helper()
	if v := rule(parseTree(t, repoRoot)); len(v) > 0 {
		t.Errorf("architecture violations:\n%s", strings.Join(v, "\n"))
	}
	got := rule(parseTree(t, violationsRoot))
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("rule on %s:\n got  %q\n want %q", violationsRoot, got, want)
	}
}

func TestArch_LayerImports(t *testing.T) {
	t.Parallel()
	check(t, layerViolations,
		"internal/authz/leak.go imports koro/internal/orgs: internal/authz is a leaf and imports no feature module (01 §5.4)",
		"internal/events/service.go imports koro/internal/orders: L8 may import only lower layers, orders is L10 (01 §5.1)",
		"internal/events/service.go imports koro/internal/talent: L8 may import only lower layers, talent is L8 (01 §5.1)",
		"internal/platform/web/web.go imports koro/internal/events: internal/platform is a leaf and imports no feature module (01 §5.4)",
		"internal/students/students.go imports koro/internal/authz/authztest: authztest is for tests only",
		"internal/students/students.go imports koro/internal/parties: L7 may import only lower layers, parties is L7 (01 §5.1)",
		"internal/widgets/widgets.go: unknown package internal/widgets: add the module to 01 §5.1 first",
	)
}

func TestArch_TalentImportBan(t *testing.T) {
	t.Parallel()
	check(t, talentViolations,
		"internal/events/service.go imports koro/internal/talent: only discovery, kvkk and cmd may (01 §5.4)",
		"internal/students/students_test.go imports koro/internal/talent: only discovery, kvkk and cmd may (01 §5.4)",
	)
}

// TestArch_OnlyDiscoveryKvkkImportTalent (BE-047, 01 §5.4): now that the
// talent package exists the import ban is not vacuous: talent is parsed,
// the composition root imports it, and outside talent itself only
// discovery, kvkk and cmd do (non-test and test files alike).
func TestArch_OnlyDiscoveryKvkkImportTalent(t *testing.T) {
	t.Parallel()
	files := parseTree(t, repoRoot)
	if v := talentViolations(files); len(v) > 0 {
		t.Fatalf("talent imported outside discovery, kvkk and cmd:\n%s", strings.Join(v, "\n"))
	}
	var talentFiles, importers []string
	for _, f := range files {
		if f.top == "talent" {
			talentFiles = append(talentFiles, f.rel)
			continue
		}
		if slices.Contains(imports(f), modPath+"talent") {
			importers = append(importers, f.top)
		}
	}
	if len(talentFiles) == 0 {
		t.Fatal("internal/talent was not parsed: the import ban checks nothing")
	}
	if !slices.Contains(importers, "cmd") {
		t.Fatal("cmd/koro does not import talent: the module is not wired")
	}
	for _, top := range importers {
		if top != "cmd" && !talentImporters[top] {
			t.Errorf("%s imports talent", top)
		}
	}
}

func TestArch_ForeignStoreBan(t *testing.T) {
	t.Parallel()
	check(t, foreignStoreViolations,
		"internal/events/service.go: students.NewMemStore: another module's store is off limits (05 §2.4-1)",
		"internal/events/service.go: st.Store: another module's store is off limits (05 §2.4-1)",
	)
}

func TestArch_ScopeOrgRequired(t *testing.T) {
	t.Parallel()
	check(t, scopeViolations,
		"internal/events/store_gorm.go: List has no org scope (ScopeOrg, scoped or a 'cross-org:' doc comment; 05 §5.5)",
	)
}

func TestSyncWritesAreAllowed(t *testing.T) {
	t.Parallel()
	allow, v := parseSyncWrites(syncWritesTxt)
	if len(v) > 0 {
		t.Fatalf("sync_writes.txt:\n%s", strings.Join(v, "\n"))
	}
	// 01 §6.3 + 05 §2.4 in full (written once, BE-004): 16 + 4 pending lines,
	// + kvkk → files.Files.Delete (06 H-49, BE-049), + feed →
	// entitlements.Entitlements.Release (BE-032: the targeted_post unit of a
	// deleted or rejected draft; 04 §2.7 deletePost "+ hak iadesi"), +
	// identity, directory, students and feed → files.Files.Delete (BE-057),
	// + outreach → entitlements.Entitlements.Release and feed →
	// outreach.Targeting.DiscardDispatch (BE-035: the dispatch unit back,
	// the dispatch of a withdrawn draft), + payments →
	// orders.Orders.ConfirmDirectPayment (BE-041: the model C direct
	// payment confirmation pays the order in its transaction), + payments →
	// orders.Orders.ApplyRefund (BE-043: a completed refund is recorded on
	// the order in its transaction), + events → files.Files.Delete (BE-050:
	// the 7-day purge of a registration export file, 01 §6.3).
	if len(allow) != 32 {
		t.Fatalf("sync_writes.txt has %d lines, want 32", len(allow))
	}
	for _, must := range []string{"kvkk files.Files.Delete", "feed entitlements.Entitlements.Release",
		"outreach entitlements.Entitlements.Release", "feed outreach.Targeting.DiscardDispatch",
		"feed outreach.Targeting.ScheduleDispatch", "outreach entitlements.Entitlements.Consume",
		"identity files.Files.Delete", "directory files.Files.Delete", "students files.Files.Delete", "feed files.Files.Delete",
		"events files.Files.Delete",
		"* audit.Logger.Log", "* files.Files.Attach", "payments orders.Orders.ApplyPayment", "payments ledger.Poster.Post",
		"payments orders.Orders.ConfirmDirectPayment", "payments orders.Orders.ApplyRefund", "payments ledger.LedgerLines.LockPayees",
		"directory orgs.OrgAdmin.GrantRole", "directory orgs.OrgAdmin.Create", "orders catalog.Offerings.Release",
	} {
		if !allow[must] {
			t.Errorf("sync_writes.txt lacks %q", must)
		}
	}
	check(t, func(files []goFile) []string { return syncWriteViolations(files, allow) },
		`internal/events/ports.go: ApplyPayment: synchronous cross-module write "orders.Orders.ApplyPayment" is not in the 01 §6.3 allow-list (sync_writes.txt)`,
		"internal/events/ports.go: CreateThing starts with a write verb: mark it '// sync-write: <module>.<Interface>.<Method>' or use an event (05 §2.4-3)",
		`internal/feed/ports.go: Record: synchronous cross-module write "consent.Recorder.Record" is not in the 01 §6.3 allow-list (sync_writes.txt)`,
	)
	// The parser rejects malformed and unknown lines.
	_, v = parseSyncWrites("events -> orders.Orders\nwidgets -> audit.Logger.Log\nfeed -> feed.Posts.Create\n* -> audit.Logger.Log\n* -> audit.Logger.Log\n")
	if len(v) != 4 {
		t.Fatalf("parseSyncWrites problems = %q, want 4", v)
	}
}

func TestArch_GormParenParams(t *testing.T) {
	t.Parallel()
	check(t, gormParamViolations,
		`internal/events/raw_sql.go: Exec: parameter directly after "(" (GORM expands it element-wise): write "( ?" or "IN ?" (06 H-30)`,
		`internal/events/raw_sql.go: Raw: parameter directly after "(" (GORM expands it element-wise): write "( ?" or "IN ?" (06 H-30)`,
		`internal/events/raw_sql.go: Where: parameter directly after "(" (GORM expands it element-wise): write "( ?" or "IN ?" (06 H-30)`,
	)
}
