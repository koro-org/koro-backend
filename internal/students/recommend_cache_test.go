package students_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"uuid"

	"koro/internal/platform/kvcache"
	"koro/internal/students"
)

// cachedEnv is newEnv with the popular clubs cache (BE-197) on the env's
// fake clock.
func cachedEnv(t *testing.T) (*env, *kvcache.Mem) {
	t.Helper()
	var c *kvcache.Mem
	e := newEnv(t, func(d *students.Deps) {
		c = kvcache.NewMem(d.Clock)
		d.Cache = c
	})
	return e, c
}

// popularPeers adds n audience eligible students of e.uniA's dept, each a
// member of club.
func (e *env) popularPeers(t *testing.T, n int, dept, club uuid.UUID) {
	t.Helper()
	for range n {
		p := students.Profile{UserID: uuid.NewV7(), UniversityID: e.uniA, DepartmentID: &dept, DegreeLevel: students.DegreeBachelor,
			Status: students.ProfileActive}
		if _, ok, err := e.store.InsertProfile(t.Context(), p); err != nil || !ok {
			t.Fatalf("InsertProfile: %v %v", ok, err)
		}
		if err := e.store.SetAudience(t.Context(), p.UserID, keysOf(e, e.uniA, dept, "2", club), true, t0); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPopularClubs_CachedPerDimensionForTTL (BE-197): the department's and
// the university's lists are cached once each (two keys, club IDs only);
// a new popular club shows after PopularClubsTTL, not before; a smaller
// limit is served from the same entry.
func TestPopularClubs_CachedPerDimensionForTTL(t *testing.T) {
	t.Parallel()
	e, c := cachedEnv(t)
	ctx := t.Context()
	a := e.profiled(t) // deptA of uniA
	clubX, clubY := uuid.NewV7(), uuid.NewV7()
	e.popularPeers(t, 10, e.deptA, clubX)

	got, err := e.recommendations(t).PopularClubs(ctx, a.UserID, 10)
	if err != nil || !slices.Equal(got.Department, []uuid.UUID{clubX}) || !slices.Equal(got.University, []uuid.UUID{clubX}) {
		t.Fatalf("first = %+v, %v", got, err)
	}
	if n := c.Keys(); n != 2 {
		t.Fatalf("cache keys = %d; want 2 (department, university)", n)
	}
	raw, found, err := c.Get(ctx, "students:popular_clubs:v1:uni:"+e.uniA.String())
	var cached []uuid.UUID
	if err != nil || !found || json.Unmarshal(raw, &cached) != nil || !slices.Equal(cached, []uuid.UUID{clubX}) {
		t.Fatalf("cached university entry = %s, %v, %v", raw, found, err)
	}

	e.popularPeers(t, 11, e.deptA, clubY)
	if got, err := e.recommendations(t).PopularClubs(ctx, a.UserID, 10); err != nil || !slices.Equal(got.Department, []uuid.UUID{clubX}) {
		t.Fatalf("within the TTL = %+v, %v; want the cached list", got, err)
	}
	e.clk.Advance(students.PopularClubsTTL)
	want := []uuid.UUID{clubY, clubX}
	if got, err := e.recommendations(t).PopularClubs(ctx, a.UserID, 10); err != nil ||
		!slices.Equal(got.Department, want) || !slices.Equal(got.University, want) {
		t.Fatalf("after the TTL = %+v, %v; want %v", got, err, want)
	}
	if got, err := e.recommendations(t).PopularClubs(ctx, a.UserID, 1); err != nil ||
		!slices.Equal(got.Department, want[:1]) || !slices.Equal(got.University, want[:1]) {
		t.Fatalf("limit 1 = %+v, %v", got, err)
	}
}

// TestPopularClubs_CacheKeepsSensitiveRule (BE-197, 03 Y2): a club that
// became sensitive after its list was cached never leaves; the answer is
// the one without the cache.
func TestPopularClubs_CacheKeepsSensitiveRule(t *testing.T) {
	t.Parallel()
	e, _ := cachedEnv(t)
	ctx := t.Context()
	a := e.profiled(t)
	clubX, clubY := uuid.NewV7(), uuid.NewV7()
	e.popularPeers(t, 11, e.deptA, clubX)
	e.popularPeers(t, 10, e.deptB, clubY)
	if got, err := e.recommendations(t).PopularClubs(ctx, a.UserID, 10); err != nil || len(got.University) != 2 {
		t.Fatalf("first = %+v, %v", got, err)
	}
	e.catalog.setSensitive(clubX, true)
	got, err := e.recommendations(t).PopularClubs(ctx, a.UserID, 10)
	if err != nil || len(got.Department) != 0 || !slices.Equal(got.University, []uuid.UUID{clubY}) {
		t.Fatalf("after the club turned sensitive = %+v, %v", got, err)
	}
}

// TestPopularClubs_CacheFailsOpen (BE-197): an unavailable cache costs the
// count, never the answer; it recovers when the cache is back.
func TestPopularClubs_CacheFailsOpen(t *testing.T) {
	t.Parallel()
	e, c := cachedEnv(t)
	ctx := t.Context()
	a := e.profiled(t)
	clubX := uuid.NewV7()
	e.popularPeers(t, 10, e.deptA, clubX)
	c.Fail(errors.New("redis down"))
	if got, err := e.recommendations(t).PopularClubs(ctx, a.UserID, 10); err != nil || !slices.Equal(got.Department, []uuid.UUID{clubX}) {
		t.Fatalf("cache down = %+v, %v", got, err)
	}
	c.Fail(nil)
	if got, err := e.recommendations(t).PopularClubs(ctx, a.UserID, 10); err != nil || !slices.Equal(got.University, []uuid.UUID{clubX}) {
		t.Fatalf("cache back = %+v, %v", got, err)
	}
	if n := c.Keys(); n != 2 {
		t.Fatalf("cache keys = %d", n)
	}
}

// TestPopularClubs_UndecodableEntryIsRecounted (BE-197): a corrupt entry
// is counted again and replaced.
func TestPopularClubs_UndecodableEntryIsRecounted(t *testing.T) {
	t.Parallel()
	e, c := cachedEnv(t)
	ctx := t.Context()
	a := e.profiled(t)
	clubX := uuid.NewV7()
	e.popularPeers(t, 10, e.deptA, clubX)
	if err := c.Set(ctx, "students:popular_clubs:v1:uni:"+e.uniA.String(), []byte("{"), students.PopularClubsTTL); err != nil {
		t.Fatal(err)
	}
	if got, err := e.recommendations(t).PopularClubs(ctx, a.UserID, 10); err != nil || !slices.Equal(got.University, []uuid.UUID{clubX}) {
		t.Fatalf("corrupt entry = %+v, %v", got, err)
	}
}
