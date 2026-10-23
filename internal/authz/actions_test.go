package authz_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"koro/internal/authz"
)

// designCounts is 03 §8: HTTP actions per module.
var designCounts = map[string]int{
	"identity": 33, "orgs": 21, "directory": 17, "students": 15, "consent": 6, "events": 35,
	"feed": 26, "outreach": 8, "messaging": 8, "discovery": 2, "entitlements": 5, "talent": 6,
	"parties": 17, "catalog": 14, "orders": 12, "payments": 23, "ledger": 10, "billing": 7,
	"notify": 8, "files": 3, "audit": 4, "kvkk": 19,
}

var actionRe = regexp.MustCompile(`^[a-z_]+\.[a-z_]+\.[a-z_]+$`)

func TestActions_CountMatchesDesign(t *testing.T) {
	t.Parallel()
	httpActions := authz.HTTPActions()
	if len(httpActions) != 299 {
		t.Fatalf("HTTP actions = %d, want 299 (03 §8)", len(httpActions))
	}
	if n := len(authz.SystemActions()); n != 21 {
		t.Fatalf("system actions = %d, want 21 (03 §3.7)", n)
	}
	// events.attendance.finalize is both an HTTP and a system action.
	if n := len(authz.Actions()); n != 299+21-1 {
		t.Fatalf("Actions() = %d unique, want 319", n)
	}
	if !slices.IsSorted(authz.Actions()) {
		t.Fatal("Actions() must be sorted")
	}
	if got := authz.Modules(); len(got) != len(designCounts) {
		t.Fatalf("Modules() = %v", got)
	}
	seen := map[authz.Action]bool{}
	for _, m := range authz.Modules() {
		acts := authz.ModuleActions(m)
		if len(acts) != designCounts[m] {
			t.Errorf("module %s: %d HTTP actions, want %d (03 §8)", m, len(acts), designCounts[m])
		}
		for _, a := range acts {
			if seen[a] {
				t.Errorf("action %s declared twice", a)
			}
			seen[a] = true
			if a.Module() != m || !actionRe.MatchString(string(a)) {
				t.Errorf("action %q is not %s.<entity>.<verb>", a, m)
			}
			if !authz.KnownAction(string(a)) || !authz.IsHTTPAction(a) {
				t.Errorf("action %s is not known as an HTTP action", a)
			}
		}
	}
	for _, a := range authz.SystemActions() {
		if !actionRe.MatchString(string(a)) || !authz.KnownAction(string(a)) || !authz.IsSystemAction(a) {
			t.Errorf("system action %q is malformed or unknown", a)
		}
		if _, ok := designCounts[a.Module()]; !ok && a.Module() != "webhook" {
			t.Errorf("system action %s: unknown module", a)
		}
		if authz.IsHTTPAction(a) && a != authz.EventsAttendanceFinalize {
			t.Errorf("system action %s is also an HTTP action", a)
		}
	}
	for _, bad := range []string{"", "events", "events.event", "Events.event.create", "events.event.fly", "nope.thing.read"} {
		if authz.KnownAction(bad) {
			t.Errorf("KnownAction(%q) = true", bad)
		}
	}
}

// TestActions_MatchDesignDoc re-reads docs/design/03-yetki-matrisi.md when it
// is reachable (koro-files checkout; CI has only the repo) and compares the
// action lists name by name, in order.
func TestActions_MatchDesignDoc(t *testing.T) {
	t.Parallel()
	doc := findDesignDoc(t)
	if doc == "" {
		t.Skip("docs/design/03-yetki-matrisi.md not reachable from the repo (CI checkout)")
	}
	b, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("^\\| `([a-z_]+\\.[a-z_]+\\.[a-z_]+)` \\|")
	sec := regexp.MustCompile(`^### 5\.(\d+) [a-z]+$`)
	var httpDoc, sysDoc []string
	in := false
	for ln := range strings.SplitSeq(string(b), "\n") {
		switch {
		case sec.MatchString(ln):
			in = true
		case strings.HasPrefix(ln, "## 6."):
			in = false
		case strings.HasPrefix(ln, "`events.registration.expire`"):
			for _, m := range regexp.MustCompile("`([a-z_]+\\.[a-z_]+\\.[a-z_]+)`").FindAllStringSubmatch(ln, -1) {
				sysDoc = append(sysDoc, m[1])
			}
		}
		if m := row.FindStringSubmatch(ln); in && m != nil {
			httpDoc = append(httpDoc, m[1])
		}
	}
	str := func(as []authz.Action) []string {
		out := make([]string, len(as))
		for i, a := range as {
			out[i] = string(a)
		}
		return out
	}
	if got := str(authz.HTTPActions()); !slices.Equal(got, httpDoc) {
		t.Errorf("HTTP actions differ from 03 §5 (%d vs %d); first difference: %s", len(got), len(httpDoc), firstDiff(got, httpDoc))
	}
	if got := str(authz.SystemActions()); !slices.Equal(got, sysDoc) {
		t.Errorf("system actions differ from 03 §3.7: %s", firstDiff(got, sysDoc))
	}
}

// TestActions_ConstantNamesFollowRule pins the Go naming rule of 03 §4.1:
// the constant is the PascalCase of every "."/"_" separated word
// (events.registration.create → EventsRegistrationCreate).
func TestActions_ConstantNamesFollowRule(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("actions_*.go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				vs := s.(*ast.ValueSpec)
				lit, _ := vs.Values[0].(*ast.BasicLit)
				v, _ := strconv.Unquote(lit.Value)
				var want strings.Builder
				for w := range strings.FieldsFuncSeq(v, func(r rune) bool { return r == '.' || r == '_' }) {
					want.WriteString(strings.ToUpper(w[:1]) + w[1:])
				}
				if vs.Names[0].Name != want.String() {
					t.Errorf("%s: constant %s = %q, want name %s", p, vs.Names[0].Name, v, want.String())
				}
				n++
			}
		}
	}
	if n != len(authz.Actions()) {
		t.Fatalf("actions_*.go declare %d constants, Actions() has %d", n, len(authz.Actions()))
	}
}

func findDesignDoc(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 7 {
		p := filepath.Join(dir, "docs", "design", "03-yetki-matrisi.md")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

func firstDiff(a, b []string) string {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return strconv.Itoa(i) + ": " + a[i] + " vs " + b[i]
		}
	}
	return "length " + strconv.Itoa(len(a)) + " vs " + strconv.Itoa(len(b))
}
