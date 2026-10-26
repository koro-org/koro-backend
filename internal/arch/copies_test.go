package arch_test

import (
	"fmt"
	"go/ast"
	"testing"
)

// platformHelpers maps a helper name that modules used to copy to its one
// platform owner (BE-111, kod-kalitesi-dry-solid §2.1): a module calls the
// platform helper and never declares its own. A module variant that really
// differs gets a name that says how.
var platformHelpers = map[string]string{
	"checkIfMatch": "httpx.CheckIfMatch",
	"etagOf":       "httpx.ETag",
	"parseID":      "httpx.ParseID",
	"optID":        "httpx.OptID",
	"optionalID":   "httpx.OptID",
	"idStr":        "httpx.IDStr",
	"reg":          "op.Register",
	"fieldErr":     "problem.Field",
	"lockIf":       "db.LockIf",
	"utcPtr":       "db.UTCPtr",
	"nullable":     "db.Nullable",
	"orgResource":  "authz.OrgResource",
	"trimSlug":     "textx.TrimSlug",
	"lengthBounds": "op.LengthBounds",
	"enumOf":       "op.Enum",
}

// testHelpers is the same list for the test doubles and assertions of
// module tests.
var testHelpers = map[string]string{
	"auditLog":    "audittest.Recorder",
	"spyTx":       "dbtest.SpyTx",
	"txOf":        "dbtest.TxOf",
	"wantErr":     "problemtest.WantErr",
	"wantProblem": "problemtest.WantProblem",
	"problemOf":   "problemtest.StatusCode",
	"rowOf":       "audittest.RowOf",
}

// copyViolations reports a top-level function or type of a feature module
// named like a platform helper.
func copyViolations(files []goFile) []string {
	var v []string
	for _, f := range files {
		if _, feature := layers[f.top]; !feature {
			continue
		}
		names := platformHelpers
		if f.test {
			names = testHelpers
		}
		for _, d := range f.ast.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil {
					continue
				}
				if owner, ok := names[d.Name.Name]; ok {
					v = append(v, fmt.Sprintf("%s: %s copies %s: call the shared helper (BE-111)", f.rel, d.Name.Name, owner))
				}
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					ts, ok := sp.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if owner, ok := names[ts.Name.Name]; ok {
						v = append(v, fmt.Sprintf("%s: %s copies %s: use the shared test double (BE-111)", f.rel, ts.Name.Name, owner))
					}
				}
			}
		}
	}
	return v
}

// TestArch_PlatformHelpersNotCopied (BE-111): the small HTTP, DB and authz
// helpers that carry the 404/412/422/428 contract, and the test doubles of
// module tests, have one owner; a module may not declare its own copy.
func TestArch_PlatformHelpersNotCopied(t *testing.T) {
	t.Parallel()
	check(t, copyViolations,
		"internal/feed/copies.go: checkIfMatch copies httpx.CheckIfMatch: call the shared helper (BE-111)",
		"internal/feed/copies.go: reg copies op.Register: call the shared helper (BE-111)",
		"internal/feed/copies_test.go: auditLog copies audittest.Recorder: use the shared test double (BE-111)",
		"internal/feed/copies_test.go: wantErr copies problemtest.WantErr: call the shared helper (BE-111)",
	)
}
