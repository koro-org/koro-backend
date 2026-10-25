package arch_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"regexp"
	"testing"
)

// batchLoopAllowed lists the files whose hand-written batch loops are not
// migrated to db.Batches yet (each with the reason). It only shrinks.
var batchLoopAllowed = map[string]string{
	"internal/consent/retention.go": "BE-105 in flight in consent; migrate after it merges (BE-112 follow-up)",
}

// batchName is a batch size constant or variable ("RetentionBatch", "jobBatch").
var batchName = regexp.MustCompile(`(?i)batch`)

// txCalls open a transaction per loop iteration.
var txCalls = map[string]bool{"WithTx": true, "WithSerializableTx": true, "inTx": true}

// batchLoopViolations reports a for loop of a feature module that opens a
// transaction and ends on a short batch ("if n < xBatch"): the loop
// db.Batches owns (BE-112), with its ctx and error rules in one place.
func batchLoopViolations(files []goFile) []string {
	var v []string
	for _, f := range files {
		if f.test || layers[f.top] == 0 || batchLoopAllowed[f.rel] != "" {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			loop, ok := n.(*ast.ForStmt)
			if !ok || !opensTx(loop.Body) || !endsOnShortBatch(loop.Body) || walksKeys(loop.Body) {
				return true
			}
			v = append(v, fmt.Sprintf("%s: %s: hand-written batch loop: use db.Batches (BE-112)", f.rel, funcOf(f.ast, loop)))
			return true
		})
	}
	return v
}

// shallow visits the nodes of body outside nested function literals and
// nested loops (a closure is a transaction body; an inner loop is its own).
func shallow(body *ast.BlockStmt, visit func(ast.Node)) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch n.(type) {
		case nil, *ast.FuncLit, *ast.ForStmt, *ast.RangeStmt:
			return false
		}
		visit(n)
		return true
	})
}

func opensTx(body *ast.BlockStmt) bool {
	found := false
	shallow(body, func(n ast.Node) {
		if c, ok := n.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && txCalls[sel.Sel.Name] {
				found = true
			}
		}
	})
	return found
}

// walksKeys reports a keyset walk ("after = rows[len(rows)-1].ID"): it
// pages through rows it does not remove, which is not db.Batches' loop.
func walksKeys(body *ast.BlockStmt) bool {
	found := false
	shallow(body, func(n ast.Node) {
		if a, ok := n.(*ast.AssignStmt); ok {
			for _, l := range a.Lhs {
				if name(l) == "after" {
					found = true
				}
			}
		}
	})
	return found
}

func endsOnShortBatch(body *ast.BlockStmt) bool {
	found := false
	shallow(body, func(n ast.Node) {
		is, ok := n.(*ast.IfStmt)
		if !ok {
			return
		}
		ast.Inspect(is.Cond, func(n ast.Node) bool {
			if b, ok := n.(*ast.BinaryExpr); ok && b.Op == token.LSS && batchName.MatchString(name(b.Y)) {
				found = true
			}
			return true
		})
	})
	return found
}

func name(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// funcOf names the top-level function that holds loop.
func funcOf(file *ast.File, loop *ast.ForStmt) string {
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && fd.Pos() <= loop.Pos() && loop.End() <= fd.End() {
			return fd.Name.Name
		}
	}
	return "?"
}

// TestArch_BatchLoopsUseDB (BE-112): a job or retention sweep that works
// in batches, one transaction each, calls db.Batches instead of writing
// its own loop (termination, ctx cancellation and error rules differed
// between the 25 copies).
func TestArch_BatchLoopsUseDB(t *testing.T) {
	t.Parallel()
	check(t, batchLoopViolations,
		"internal/feed/batches.go: purge: hand-written batch loop: use db.Batches (BE-112)",
	)
}
