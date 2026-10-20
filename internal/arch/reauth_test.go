package arch_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A refused re-authentication proof of an R operation is the operation's
// denied record (03 §6.2; BE-072, BE-074, BE-075): every module hands the
// error of its proof check to audit.ReauthFailed, which writes the denied
// reauth_failed record for a verdict (403 reauth_required/reauth_failed,
// 429) and nothing for an error without one (an overload: 503). An R
// operation that returns the error as it came answers the same 403 or 429
// and leaves no trace of the refused attempt; the rule keeps a new one
// (events.export.create, talent's, students') from doing so.
//
// The rule is syntactic and local, so that a failure points at one line:
// a proof check is assigned (`kind, err := s.d.Reauth.Verify(…)`, also in
// an if statement's init) and directly followed by
// `if err != nil { …; return …, audit.ReauthFailed(…, err) }` whose every
// return goes through audit.ReauthFailed. A module helper that does this
// once (payments' reauth, orgs' reauth …) is enough: its callers return
// what it returns. A module that registers an R operation
// (op.Meta{Reauth: true}) checks at least one proof this way.

// reauthOwn is the proof check of the module that implements the port:
// identity checks its own R operations' proofs with its unexported verify;
// every other module calls identity's Verify through its Reauth port.
var reauthOwn = map[string]string{"identity": "verify"}

// reauthDirect are the deliberate exceptions, "<file> <func>" of a proof
// check whose error goes back as it came, with the reason.
var reauthDirect = map[string]string{
	// identity's Verify is the port itself: the calling module writes the
	// denied record of its own action (audit.ReauthFailed); a record here
	// would be a second one for the same refusal (BE-074, BE-075).
	"internal/identity/account.go Verify": "the reauth port: its caller audits",
}

const (
	auditPkg = modPath + "audit"
	opPkg    = modPath + "platform/httpx/op"
)

// importName is the local name of path in f ("" when f does not import it).
func importName(f goFile, path string) string {
	for _, imp := range f.ast.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == path {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return path[strings.LastIndex(path, "/")+1:]
		}
	}
	return ""
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && name != "" && id.Name == name
}

// reauthCallee names call when it checks a re-authentication proof, ""
// otherwise: the module's reauth port, a Verify method of a field named
// Reauth (s.d.Reauth.Verify) or a function held in one (s.d.Reauth), or
// the module's own check (reauthOwn).
func reauthCallee(top string, call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	field, isField := sel.X.(*ast.SelectorExpr)
	switch {
	case sel.Sel.Name == "Reauth",
		sel.Sel.Name == "Verify" && isField && field.Sel.Name == "Reauth",
		reauthOwn[top] != "" && sel.Sel.Name == reauthOwn[top]:
		return types.ExprString(sel)
	}
	return ""
}

// reauthFailedCall reports whether e is audit.ReauthFailed(…, errName).
func reauthFailedCall(e ast.Expr, errName, auditName string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "ReauthFailed" && isIdent(sel.X, auditName) && isIdent(call.Args[len(call.Args)-1], errName)
}

// routedIf reports whether ifs is `if errName != nil { … }` whose body
// ends in a return and whose every return (closures aside) has
// audit.ReauthFailed(…, errName) as its last result.
func routedIf(ifs *ast.IfStmt, errName, auditName string) bool {
	cond, ok := ifs.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ || !isIdent(cond.X, errName) || !isIdent(cond.Y, "nil") || len(ifs.Body.List) == 0 {
		return false
	}
	if _, ok := ifs.Body.List[len(ifs.Body.List)-1].(*ast.ReturnStmt); !ok {
		return false
	}
	routed := true
	ast.Inspect(ifs.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			routed = routed && len(x.Results) > 0 && reauthFailedCall(x.Results[len(x.Results)-1], errName, auditName)
		}
		return true
	})
	return routed
}

// checkedProof returns the proof check of s and the name of its error:
// `…, err := <proof check>(…)` (or =) with a named error.
func checkedProof(top string, s ast.Stmt) (*ast.CallExpr, string) {
	as, ok := s.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
		return nil, ""
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok || reauthCallee(top, call) == "" {
		return nil, ""
	}
	errID, ok := as.Lhs[len(as.Lhs)-1].(*ast.Ident)
	if !ok || errID.Name == "_" {
		return nil, ""
	}
	return call, errID.Name
}

// stmtLists returns every statement list under n.
func stmtLists(n ast.Node) [][]ast.Stmt {
	var out [][]ast.Stmt
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BlockStmt:
			out = append(out, x.List)
		case *ast.CaseClause:
			out = append(out, x.Body)
		case *ast.CommClause:
			out = append(out, x.Body)
		}
		return true
	})
	return out
}

// declaresROperation reports whether f registers an R operation:
// an op.Meta literal with Reauth: true.
func declaresROperation(f goFile) bool {
	opName, found := importName(f, opPkg), false
	ast.Inspect(f.ast, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || found {
			return !found
		}
		if sel, ok := lit.Type.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Meta" || !isIdent(sel.X, opName) {
			return true
		}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok && isIdent(kv.Key, "Reauth") && isIdent(kv.Value, "true") {
				found = true
			}
		}
		return !found
	})
	return found
}

// reauthViolations is the rule on the non-test files of the feature
// modules; used lists the reauthDirect entries that matched.
func reauthViolations(files []goFile, allow map[string]string) (v, used []string) {
	rOps, checks := map[string]bool{}, map[string]int{}
	for _, f := range files {
		if f.test || layers[f.top] == 0 {
			continue // tests, cmd, platform, authz, arch
		}
		rOps[f.top] = rOps[f.top] || declaresROperation(f)
		auditName := importName(f, auditPkg)
		for _, d := range f.ast.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			routed := map[*ast.CallExpr]bool{}
			for _, list := range stmtLists(fd.Body) {
				for i, s := range list {
					if ifs, ok := s.(*ast.IfStmt); ok && ifs.Init != nil {
						if call, errName := checkedProof(f.top, ifs.Init); call != nil && routedIf(ifs, errName, auditName) {
							routed[call] = true
						}
					}
					if call, errName := checkedProof(f.top, s); call != nil && i+1 < len(list) {
						if ifs, ok := list[i+1].(*ast.IfStmt); ok && ifs.Init == nil && routedIf(ifs, errName, auditName) {
							routed[call] = true
						}
					}
				}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := reauthCallee(f.top, call)
				if callee == "" {
					return true
				}
				checks[f.top]++
				key := f.rel + " " + fd.Name.Name
				_, allowed := allow[key]
				switch {
				case routed[call]:
				case allowed:
					used = append(used, key)
				default:
					v = append(v, fmt.Sprintf("%s: %s: the error of %s does not return through audit.ReauthFailed: "+
						"a refused proof is the operation's denied record (03 §6.2, BE-075)", f.rel, fd.Name.Name, callee))
				}
				return true
			})
		}
	}
	for top, r := range rOps {
		if r && checks[top] == 0 {
			v = append(v, fmt.Sprintf("internal/%s: registers an R operation (op.Meta Reauth) but checks no proof through "+
				"its reauth port (Reauth.Verify + audit.ReauthFailed; 03 §6.2, BE-075)", top))
		}
	}
	return v, used
}

func TestArch_ReauthFailureAudited(t *testing.T) {
	t.Parallel()
	v, used := reauthViolations(parseTree(t, repoRoot), reauthDirect)
	if len(v) > 0 {
		t.Errorf("a refused re-authentication proof must be audited:\n%s", strings.Join(v, "\n"))
	}
	for key := range reauthDirect {
		if !slices.Contains(used, key) {
			t.Errorf("reauthDirect entry %q matches no proof check: remove it", key)
		}
	}
	// The rule still sees the modules that check proofs today (BE-074):
	// a renamed port or helper must not silently empty it.
	checked := map[string]bool{}
	for _, f := range parseTree(t, repoRoot) {
		if !f.test && layers[f.top] > 0 {
			ast.Inspect(f.ast, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && reauthCallee(f.top, call) != "" {
					checked[f.top] = true
				}
				return true
			})
		}
	}
	for _, m := range []string{"identity", "consent", "orgs", "directory", "parties", "catalog", "billing", "ledger", "payments", "kvkk"} {
		if !checked[m] {
			t.Errorf("no proof check found in internal/%s: the rule no longer matches its reauth port", m)
		}
	}

	const tail = " does not return through audit.ReauthFailed: a refused proof is the operation's denied record (03 §6.2, BE-075)"
	got, _ := reauthViolations(parseTree(t, violationsRoot), reauthDirect)
	want := []string{
		"internal/events/export.go: CreateExport: the error of s.d.Reauth.Verify" + tail,
		"internal/events/export.go: ExportClosure: the error of s.d.Reauth.Verify" + tail,
		"internal/events/export.go: ExportDirect: the error of s.d.Reauth.Verify" + tail,
		"internal/events/export.go: ExportDropped: the error of s.d.Reauth.Verify" + tail,
		"internal/events/export.go: ExportLater: the error of s.d.Reauth.Verify" + tail,
		"internal/events/export.go: ExportOtherErr: the error of s.d.Reauth.Verify" + tail,
		"internal/events/export.go: ExportPartly: the error of s.d.Reauth.Verify" + tail,
		"internal/feed/reauth.go: Pin: the error of s.d.Reauth" + tail,
		"internal/identity/mfa.go: DisableTOTP: the error of s.verify" + tail,
		"internal/students: registers an R operation (op.Meta Reauth) but checks no proof through its reauth port " +
			"(Reauth.Verify + audit.ReauthFailed; 03 §6.2, BE-075)",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("rule on %s:\n got  %q\n want %q", violationsRoot, got, want)
	}
}
