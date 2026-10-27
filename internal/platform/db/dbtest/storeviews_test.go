package dbtest_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"koro/internal/platform/db/dbtest"
)

func TestStoreViewViolations_FindsWholeStoreOutsideAllowedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"store.go":   "package m\n\nfunc (s *service) posts() postStore { return s.d.Store }\n",
		"privacy.go": "package m\n\nfunc (p provider) erase() { p.s.d.Store.EraseUser(ctx, id) }\n",
		"write.go": "package m\n\n" +
			"// s.d.Store.InsertPost is fine in a comment\n" +
			"func (s *service) a() { s.posts().InsertPost(ctx, p) }\n" +
			"func (s *service) b() { s.d.Store.InsertPost(ctx, p) }\n" +
			"func (s *service) c() {\n\tst := s.d.Store\n\t_ = st\n}\n" +
			"func (w sweeper) d() { w.s.d.Store.Purge(ctx) }\n",
		"write_test.go": "package m\n\nfunc x() { s.d.Store.InsertPost(ctx, p) }\n",
		"notes.txt":     "s.d.Store.InsertPost\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := dbtest.StoreViewViolations(dir, "store.go", "privacy.go")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"write.go:5: func (s *service) b() { s.d.Store.InsertPost(ctx, p) }",
		"write.go:7: st := s.d.Store",
		"write.go:10: func (w sweeper) d() { w.s.d.Store.Purge(ctx) }",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("violations = %q, want %q", got, want)
	}
	all, err := dbtest.StoreViewViolations(dir)
	if err != nil {
		t.Fatal(err)
	}
	// privacy.go reaches the whole Store; the one-line accessor of store.go
	// returns it and calls nothing on it.
	if len(all) != len(want)+1 || all[0] != "privacy.go:3: func (p provider) erase() { p.s.d.Store.EraseUser(ctx, id) }" {
		t.Fatalf("without allowed files: %q", all)
	}
}
