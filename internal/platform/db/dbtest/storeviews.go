package dbtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// wholeStore matches a call on the whole Store of a module's Deps
// (d.Store.Method, also s.d.Store and w.s.d.Store) and an assignment of it
// (st := s.d.Store).
var wholeStore = regexp.MustCompile(`\bd\.Store\.[A-Z]|=\s*[\w.]*\bd\.Store\s*$`)

// StoreViewViolations returns the lines of the non-test Go files of dir
// that reach the whole Store of the module, as "file:line: code"; the
// files named in allowed (base names) and comment lines are skipped.
func StoreViewViolations(dir string, allowed ...string) ([]string, error) {
	skip := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		skip[a] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || skip[name] {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if wholeStore.MatchString(line) {
				out = append(out, name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	return out, nil
}

// CheckStoreViews keeps the split of a module's Store into embedded views
// (05 §2.2; BE-142, BE-143, BE-146): a service file of the package under
// test (the working directory of its test) calls the store only through
// the view of its aggregate (the accessors in store.go), a background
// component through the views it holds. Only the files in allowed (the
// accessors themselves, the KVKK provider whose export and erasure cover
// every table) hold the whole Store. Every other line that does fails t.
func CheckStoreViews(t testing.TB, allowed ...string) {
	t.Helper()
	lines, err := StoreViewViolations(".", allowed...)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		t.Errorf("%s reaches the whole Store; use the view of its aggregate (store.go)", l)
	}
}
