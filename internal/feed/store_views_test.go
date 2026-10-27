package feed_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestStore_ReachedThroughNarrowViews keeps the BE-143 split (05 §2.2): a
// service file calls the store only through the view of its aggregate
// (s.posts(), s.media(), s.settings(), s.interactions(), s.reports(),
// s.moderation(); store.go), the retention sweeper through the views it
// holds (retention.go). Only the accessors themselves and the KVKK
// provider (privacy.go: export and erasure cover every table) hold the
// whole Store.
func TestStore_ReachedThroughNarrowViews(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{"store.go": true, "privacy.go": true}
	whole := regexp.MustCompile(`\bd\.Store\.[A-Z]|=\s*[\w.]*\bd\.Store\s*$`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || allowed[name] {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if whole.MatchString(line) {
				t.Errorf("%s:%d reaches the whole Store; use the view of its aggregate (store.go): %s",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}
