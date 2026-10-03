// Package platform_test checks the leaf rule of the BE-003 packages
// (05 §1.3, 01 §5.1): they import no domain or platform package, so every
// module can use them without cycles. internal/arch (BE-004) enforces the
// layer rules for the whole tree; this test pins the narrower leaf rule.
package platform_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// leafPackages lists the leaf packages and the koro packages each may import
// (page returns a platform/problem error, itself a stdlib-only leaf).
var leafPackages = map[string][]string{
	"money":         nil,
	"iban":          nil,
	"iban/ibantest": nil,
	"civil":         nil,
	"textx":         nil,
	"page":          {"koro/internal/platform/problem"},
	"token":         nil,
	"passwd":        nil,
	"crypto":        nil,
	"sanitize":      nil,
}

// thirdParty is the complete list of non-stdlib imports allowed in leaves.
var thirdParty = []string{
	"golang.org/x/crypto/",
	"golang.org/x/text/",
	"github.com/microcosm-cc/bluemonday",
}

func TestLeafPackages_ImportNoKoroPackage(t *testing.T) {
	t.Parallel()
	for dir, allowed := range leafPackages {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			files, err := filepath.Glob(filepath.Join(dir, "*.go"))
			if err != nil || len(files) == 0 {
				t.Fatalf("no Go files in %s (%v)", dir, err)
			}
			for _, f := range files {
				if strings.HasSuffix(f, "_test.go") {
					continue
				}
				src, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				af, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
				if err != nil {
					t.Fatal(err)
				}
				for _, imp := range af.Imports {
					path, _ := strconv.Unquote(imp.Path.Value)
					if !importAllowed(path, allowed) {
						t.Errorf("%s imports %q: leaf packages may import only the standard library, %s%s",
							f, path, strings.Join(thirdParty, ", "), allowedText(allowed))
					}
				}
			}
		})
	}
}

func TestLeafPackages_RuleRejects(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"koro/internal/platform/clock", "koro/internal/identity", "github.com/google/uuid", "golang.org/x/net/html"} {
		if importAllowed(path, nil) {
			t.Errorf("importAllowed(%q) = true", path)
		}
	}
	for _, path := range []string{"crypto/sha256", "uuid", "golang.org/x/crypto/argon2", "github.com/microcosm-cc/bluemonday"} {
		if !importAllowed(path, nil) {
			t.Errorf("importAllowed(%q) = false", path)
		}
	}
}

func importAllowed(path string, allowed []string) bool {
	if path == "koro" || strings.HasPrefix(path, "koro/") {
		for _, a := range allowed {
			if path == a {
				return true
			}
		}
		return false
	}
	first, _, _ := strings.Cut(path, "/")
	if !strings.Contains(first, ".") {
		return true // standard library
	}
	for _, p := range thirdParty {
		if path == strings.TrimSuffix(p, "/") || strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func allowedText(allowed []string) string {
	if len(allowed) == 0 {
		return ""
	}
	return " and " + strings.Join(allowed, ", ")
}
