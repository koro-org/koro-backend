package main

import (
	"io/fs"
	"maps"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"weak"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"
	"go.yaml.in/yaml/v3"

	"koro/internal/authz/authztest"
	"koro/internal/platform/httpx"
)

// TestAuthzMatrix is the authz matrix of the real application (03 §7.4):
// every OpenAPI operation × 18 personas, the non-API routes and the
// scenarios, with the static checks 1–9 first. An operation missing from
// testdata/authz/<module>.yaml fails here.
func TestAuthzMatrix(t *testing.T) {
	t.Parallel()
	env := &matrixEnv{opts: realMatrixOptions()}
	env.run(t, os.DirFS("testdata/authz"))
}

// TestAuthzMatrix_SelfTest runs the whole harness on the synthetic testonly
// module: 401 (middleware), 404 (invisible org surface, missing resource,
// flag off), 403 (visible without the role, admin surface, real authz.Can
// default deny), 2xx, a config variant, a WebSocket non-API route, fixtures,
// a body with placeholders, an Idempotency-Key and four scenarios (four
// eyes, MFA, hook, config).
func TestAuthzMatrix_SelfTest(t *testing.T) {
	t.Parallel()
	selfTestEnv().run(t, selfTestFS(t))
}

func TestAuthzMatrix_MissingOperation_Fails(t *testing.T) {
	t.Parallel()
	fsys := selfTestFS(t)
	// Drop testOnlyDefaultDeny from the matrix: the spec still has it.
	b := string(fsys["testonly.yaml"].Data)
	i := strings.Index(b, "  testOnlyDefaultDeny:")
	j := strings.Index(b, "non_api_routes:")
	probs := selfTestProblems(t, edit(t, fsys, "testonly.yaml", b[i:j], ""), nil)
	wantProblem(t, probs, `operation "testOnlyDefaultDeny" (GET /api/v1/testonly/default-deny, tag testonly) is missing from the authz matrix`)
}

func TestAuthzMatrix_StaleEntry_Fails(t *testing.T) {
	t.Parallel()
	ghost := `  testOnlyGhost:
    method: GET
    path: /api/v1/testonly/ghost
    action: testonly.thing.read
    expect: { anonymous: 401, "@signed_in": 404 }

non_api_routes:`
	probs := selfTestProblems(t, edit(t, selfTestFS(t), "testonly.yaml", "non_api_routes:", ghost), nil)
	wantProblem(t, probs, `stale matrix entry "testOnlyGhost" (testonly.yaml): not in the OpenAPI spec`)

	// A non-API route that no longer exists is stale too.
	route := "non_api_routes:\n  - { method: GET, path: /api/v1/testonly/gone, expect: { \"@all\": 200 } }"
	probs = selfTestProblems(t, edit(t, selfTestFS(t), "testonly.yaml", "non_api_routes:", route), nil)
	wantProblem(t, probs, "stale non_api_routes entry GET /api/v1/testonly/gone")
}

func TestAuthzMatrix_DuplicateAcrossFiles_Fails(t *testing.T) {
	t.Parallel()
	fsys := selfTestFS(t)
	dup := `version: 1
operations:
  testOnlyListThings:
    method: GET
    path: /api/v1/testonly/things
    action: testonly.thing.list
    public: true
    expect: { "@all": 200 }
`
	env := selfTestEnv()
	env.opts.modules = append(env.opts.modules, "testonly2")
	_, probs := loadMatrix(edit(t, fsys, "testonly2.yaml", "", dup), env.opts.modules)
	wantProblem(t, probs, `operation "testOnlyListThings" is declared in both testonly.yaml and testonly2.yaml`)

	// The same holds for fixtures and scenarios.
	fx := "version: 1\nfixtures:\n  thing_a: { kind: thing, org: club_b, created_by: other_club_admin }\n"
	_, probs = loadMatrix(edit(t, fsys, "fixtures/testonly2.yaml", "", fx), env.opts.modules)
	wantProblem(t, probs, `fixture "thing_a" is declared in both fixtures/testonly.yaml and fixtures/testonly2.yaml`)

	// A module file name must be a module (R3).
	_, probs = loadMatrix(edit(t, fsys, "events_extra.yaml", "", dup), env.opts.modules)
	wantProblem(t, probs, "events_extra.yaml: no such module")
}

func TestAuthzMatrix_PersonaCoverage_Exactly18(t *testing.T) {
	t.Parallel()
	// The real _personas.yaml is 03 §1.1 (16 roles) + the 2 IDOR personas.
	design := []string{
		"anonymous", "user_unverified", "student", "student_minor", "club_member", "club_admin",
		"other_club_admin", "event_staff", "university_rep", "company_member", "company_admin",
		"other_company_member", "moderator", "support", "dpo", "platform_admin", "finance_admin",
		"talent_auditor",
	}
	b, err := os.ReadFile("testdata/authz/_personas.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var pf struct {
		Personas []string            `yaml:"personas"`
		Groups   map[string][]string `yaml:"groups"`
	}
	if err := yaml.Unmarshal(b, &pf); err != nil {
		t.Fatal(err)
	}
	if len(pf.Personas) != 18 || !slices.Equal(pf.Personas, design) || !slices.Equal(authztest.PersonaNames, design) {
		t.Fatalf("_personas.yaml %v / authztest %v must equal 03 §1.1 + IDOR %v", pf.Personas, authztest.PersonaNames, design)
	}
	if len(pf.Groups["signed_in"]) != 17 || slices.Contains(pf.Groups["signed_in"], "anonymous") {
		t.Fatalf("@signed_in must be every persona except anonymous: %v", pf.Groups["signed_in"])
	}

	fsys := selfTestFS(t)
	cases := []struct {
		name, file, old, new, want string
	}{
		{"missing persona", "testonly.yaml", "      club_member: 403           # member, role too weak\n", "",
			`operation "testOnlyGetThing": no expectation for persona club_member`},
		{"persona twice via a group", "testonly.yaml", "      club_admin: 201\n", "      club_admin: 201\n      student: 404\n",
			`operation "testOnlyCreateThing": persona student appears more than once`},
		{"unknown persona", "testonly.yaml", "      club_admin: 201\n", "      club_admin: 201\n      root: 200\n",
			`unknown persona "root"`},
		{"@all on a protected operation", "testonly.yaml", `expect: { anonymous: 401, "@signed_in": 403 }`, `expect: { "@all": 403 }`,
			"@all is allowed only on public operations"},
		{"a persona dropped from _personas.yaml", "_personas.yaml", "talent_auditor]\ngroups:", "]\ngroups:",
			"differ from the 18 personas"},
		{"422 without a note", "testonly.yaml", "      club_admin: 201\n", "      club_admin: 422\n",
			"club_admin expects 422 without a note"},
		{"unknown group", "testonly.yaml", `"@outsiders_a": 404        # club_a`, `"@outsiders_x": 404        # club_a`,
			"unknown group @outsiders_x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			wantProblem(t, selfTestProblems(t, edit(t, fsys, c.file, c.old, c.new), nil), c.want)
		})
	}
}

func TestAuthzMatrix_PublicFlagMatchesSecurity(t *testing.T) {
	t.Parallel()
	fsys := selfTestFS(t)
	// A protected operation marked public.
	probs := selfTestProblems(t, edit(t, fsys, "testonly.yaml",
		"    action: testonly.thing.deny\n", "    action: testonly.thing.deny\n    public: true\n"), nil)
	wantProblem(t, probs, `operation "testOnlyDefaultDeny": empty Security=false but matrix public=true`)
	// A public operation without public: true.
	probs = selfTestProblems(t, edit(t, fsys, "testonly.yaml",
		"    action: testonly.thing.list\n    public: true\n", "    action: testonly.thing.list\n"), nil)
	wantProblem(t, probs, `operation "testOnlyListThings": empty Security=true but matrix public=false`)
	// The unchanged self-test matrix is consistent.
	if probs := selfTestProblems(t, fsys, nil); len(probs) > 0 {
		t.Fatalf("self-test matrix: %v", probs)
	}
}

// TestAuthzMatrix_WrongExpectedCode_Fails proves that check 10 compares the
// real response: a wrong cell fails the case.
func TestAuthzMatrix_WrongExpectedCode_Fails(t *testing.T) {
	t.Parallel()
	env := selfTestEnv()
	fsys := edit(t, selfTestFS(t), "testonly.yaml", "      club_admin: 200\n", "      club_admin: 403\n")
	m, probs := loadMatrix(fsys, env.opts.modules)
	if len(probs) > 0 {
		t.Fatal(probs)
	}
	env.m = m
	ran := false
	for _, c := range env.cases() {
		if c.name != "testOnlyGetThing/club_admin" {
			continue
		}
		ran = true
		if got, _ := env.runCase(t, c); got == c.want || got != http.StatusOK {
			t.Fatalf("club_admin got %d with expectation %d: a wrong cell must not pass", got, c.want)
		}
	}
	if !ran {
		t.Fatal("case testOnlyGetThing/club_admin not generated")
	}
}

func TestAuthzMatrix_ActionCoverageAndRoutes(t *testing.T) {
	t.Parallel()
	fsys := selfTestFS(t)
	cases := []struct {
		name  string
		fsys  func() fs.FS
		extra func(a *matrixApp)
		want  string
	}{
		{"unbound action without pending entry", func() fs.FS {
			return edit(t, fsys, "testonly.yaml", "  - testonly.thing.archive       # bound by a later ticket (illustration)\n", "")
		}, nil, "action testonly.thing.archive is bound to no operation"},
		{"stale pending entry", func() fs.FS {
			return edit(t, fsys, "testonly.yaml", "  - testonly.thing.archive", "  - testonly.thing.archive\n  - testonly.thing.read")
		}, nil, "pending action testonly.thing.read is bound now"},
		{"action mismatch", func() fs.FS {
			return edit(t, fsys, "testonly.yaml", "    action: testonly.thing.deny\n", "    action: testonly.thing.read\n")
		}, nil, `x-koro-action "testonly.thing.deny", matrix action "testonly.thing.read"`},
		{"idempotency mismatch", func() fs.FS {
			return edit(t, fsys, "testonly.yaml", "    idempotency: required\n", "")
		}, nil, `x-koro-idempotency "required" but matrix idempotency ""`},
		{"unlisted gin route", func() fs.FS { return fsys }, func(a *matrixApp) {
			a.app.Engine.GET(httpx.APIPrefix+"/testonly/raw", func(c *gin.Context) { c.Status(http.StatusOK) })
		}, "gin route GET /api/v1/testonly/raw is neither a huma operation nor in non_api_routes"},
		{"unknown fixture", func() fs.FS {
			return edit(t, fsys, "testonly.yaml", "thing_id: thing_a }", "thing_id: thing_zz }")
		}, nil, `unknown fixture "thing_zz"`},
		{"fixture shadows a persona", func() fs.FS {
			return edit(t, fsys, "fixtures/testonly.yaml", "  thing_a:", "  student:")
		}, nil, `"student" is a persona, helper user or org fixture of authztest`},
		{"missing path parameter", func() fs.FS {
			return edit(t, fsys, "testonly.yaml", "    path_params: { thing_id: thing_pending }\n", "")
		}, nil, "path parameter thing_id has no path_params entry"},
		{"config key without default", func() fs.FS {
			return edit(t, fsys, "testonly.yaml", "config: { testonly_flag: true }", "config: { testonly_flagg: true }")
		}, nil, `config key "testonly_flagg" has no default`},
		{"scenario without a negative step", func() fs.FS {
			return edit(t, fsys, "scenarios/testonly.yaml", "expect: 403, code: forbidden", "expect: 200")
		}, nil, "needs at least one positive (2xx) and one negative (4xx) step"},
		{"unknown YAML field", func() fs.FS {
			return edit(t, fsys, "testonly.yaml", "    action: testonly.thing.deny\n", "    action: testonly.thing.deny\n    expected: {}\n")
		}, nil, "field expected not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			wantProblem(t, selfTestProblems(t, c.fsys(), c.extra), c.want)
		})
	}
}

// TestAuthzMatrix_CasePartition pins the BE-060 split of the real matrix:
// every case runs exactly once, either on the shared application of its
// config or on a fresh one, and every case that may change state (a
// non-GET expected to succeed or to fail in the service) is fresh.
func TestAuthzMatrix_CasePartition(t *testing.T) {
	t.Parallel()
	env := &matrixEnv{opts: realMatrixOptions()}
	m, probs := loadMatrix(os.DirFS("testdata/authz"), env.opts.modules)
	if len(probs) > 0 {
		t.Fatal(probs)
	}
	env.m = m
	all := env.cases()
	for _, allFresh := range []bool{false, true} {
		groups, fresh := partitionCases(all, allFresh)
		seen := map[string]int{}
		for _, c := range fresh {
			seen[c.name]++
		}
		shared := 0
		for _, g := range groups {
			for _, c := range append(slices.Clone(g.reads), g.denials...) {
				seen[c.name]++
				shared++
				if !c.sharedSafe() || configKey(c.config) != g.key {
					t.Errorf("%s: on shared application %q but sharedSafe=%v config %q", c.name, g.key, c.sharedSafe(), configKey(c.config))
				}
				if c.read() != slices.ContainsFunc(g.reads, func(r matrixCase) bool { return r.name == c.name }) {
					t.Errorf("%s: read/denial phase mismatch", c.name)
				}
			}
		}
		for _, c := range all {
			if seen[c.name] != 1 {
				t.Errorf("case %s runs %d times (allFresh=%v)", c.name, seen[c.name], allFresh)
			}
			if !c.read() && c.want/100 == 2 && !slices.ContainsFunc(fresh, func(f matrixCase) bool { return f.name == c.name }) {
				t.Errorf("%s: a successful write must run on a fresh application", c.name)
			}
		}
		if len(seen) != len(all) || shared+len(fresh) != len(all) {
			t.Errorf("partition of %d cases: %d shared + %d fresh, %d distinct", len(all), shared, len(fresh), len(seen))
		}
		if allFresh && (len(groups) != 0 || len(fresh) != len(all)) {
			t.Errorf("-matrix.fresh must run every case fresh: %d groups, %d fresh", len(groups), len(fresh))
		}
		if !allFresh {
			t.Logf("%d cases: %d shared on %d applications, %d fresh; %d scenarios", len(all), shared, len(groups), len(fresh), len(m.scenarios))
		}
	}
}

// TestAuthzMatrix_AppsAreReleased: once its test is over, a matrix
// application is garbage (BE-073). A package-level value that keeps a
// container reachable (a map keyed by wiring pointer, a registry, a
// goroutine) keeps every application of the real matrix alive until the end
// of the run: ≈ 1700 containers, gigabytes under -race, and a heap above
// GOMEMLIMIT that the GC chases without end. One application per distinct
// config of the real matrix (cases and scenarios), each serving one read in
// a parallel subtest, as the fresh cases do.
func TestAuthzMatrix_AppsAreReleased(t *testing.T) {
	t.Parallel()
	env := &matrixEnv{opts: realMatrixOptions()}
	m, probs := loadMatrix(os.DirFS("testdata/authz"), env.opts.modules)
	if len(probs) > 0 {
		t.Fatal(probs)
	}
	env.m = m
	configs, reads := map[string]map[string]any{}, map[string]matrixCase{}
	for _, c := range env.cases() {
		k := configKey(c.config)
		configs[k] = c.config
		if _, ok := reads[k]; !ok && c.read() && !c.websocket {
			reads[k] = c
		}
	}
	for _, s := range m.scenarios {
		cfg := mergeConfig(m.config, s.Config)
		configs[configKey(cfg)] = cfg
	}
	keys := slices.Sorted(maps.Keys(configs))
	held := make([]weak.Pointer[container], len(keys))
	// Checked once every subtest is done but before this test ends, the
	// state of the real matrix at its end: the parent keeps its finished
	// parallel subtests (and what they still reference, such as the
	// t.Cleanup functions they registered) until it returns.
	t.Cleanup(func() {
		alive := func() (out []string) {
			for i, w := range held {
				if w.Value() != nil {
					out = append(out, keys[i])
				}
			}
			return out
		}
		for range 3 {
			if runtime.GC(); len(alive()) == 0 {
				break
			}
		}
		if left := alive(); len(left) > 0 {
			t.Errorf("%d of %d matrix applications are still reachable after their subtest (configs %q): a package-level "+
				"value or a function the finished subtest keeps (t.Cleanup) holds the container; release it in "+
				"releaseMatrixApp (matrix_harness_test.go)", len(left), len(keys), left)
			return
		}
		t.Logf("%d applications (one per config) released", len(keys))
	})
	for i, k := range keys {
		t.Run(k, func(t *testing.T) {
			t.Parallel() // as the fresh cases of TestAuthzMatrix
			a := env.newMatrixApp(t, configs[k])
			if c, ok := reads[k]; ok {
				a.do(t, m, c)
			}
			held[i] = weak.Make(a.c)
		})
	}
}

// selfTestSharedCases is the fixed subset of the self-test matrix that the
// shared-application self-tests (isolation, leak, status-only) run. They
// prove properties of runShared, not of the synthetic module, which
// TestAuthzMatrix_SelfTest runs in full; BE-063 cut them from 231 cases
// (and one fresh application per case) to this list. It keeps both config
// groups, the reads whose body a leaky denial changes (the list), the org,
// variant, flag and WebSocket reads, and denials by 401, 403 and 404 on the
// org and admin surfaces (the leaky create among them). A name that is no
// longer generated fails the test.
var selfTestSharedCases = []string{
	// reads, config testonly_flag=false
	"testOnlyListThings/anonymous",
	"testOnlyListThings/student",
	"testOnlyGetThing/anonymous",
	"testOnlyGetThing/student",
	"testOnlyGetThing/club_member",
	"testOnlyGetThing/club_admin",
	"testOnlyGetThing/missing_thing/club_admin",
	"testOnlyGetThing/company_thing/company_admin",
	"testOnlyGetFlagged/talent_auditor",
	"testOnlyDefaultDeny/platform_admin",
	"route GET /api/v1/testonly/live/club_admin",
	"route GET /api/v1/testonly/live/club_member",
	// denials, config testonly_flag=false
	"testOnlyCreateThing/anonymous",
	"testOnlyCreateThing/student",
	"testOnlyCreateThing/club_member",
	"testOnlyCreateThing/moderator",
	"testOnlyAdminCreateThing/student",
	"testOnlyApproveThing/support",
	// reads, config testonly_flag=true
	"testOnlyGetFlagged/flag_on/talent_auditor",
	"testOnlyGetFlagged/flag_on/student",
}

// selfTestSharedGroups returns the shared groups of the selfTestSharedCases
// of env.m: every name must be a generated, shared-safe case, and every
// config of the self-test matrix keeps a group.
func selfTestSharedGroups(t *testing.T, env *matrixEnv) []*sharedGroup {
	t.Helper()
	all := env.cases()
	byName := make(map[string]matrixCase, len(all))
	configs := map[string]bool{}
	for _, c := range all {
		byName[c.name] = c
		configs[configKey(c.config)] = true
	}
	picked := make([]matrixCase, 0, len(selfTestSharedCases))
	for _, n := range selfTestSharedCases {
		c, ok := byName[n]
		if !ok {
			t.Fatalf("selfTestSharedCases: case %q is not generated by the self-test matrix", n)
		}
		if !c.sharedSafe() {
			t.Fatalf("selfTestSharedCases: case %q is not shared-safe", n)
		}
		picked = append(picked, c)
	}
	groups, fresh := partitionCases(picked, false)
	if len(fresh) != 0 || len(groups) != len(configs) {
		t.Fatalf("selfTestSharedCases: %d shared groups for %d configs, %d fresh cases", len(groups), len(configs), len(fresh))
	}
	for _, g := range groups {
		if len(g.reads) == 0 {
			t.Fatalf("selfTestSharedCases: config %s has no read", g.key)
		}
	}
	return groups
}

// selfTestSharedRun loads the self-test matrix into env and runs the shared
// groups of selfTestSharedCases with seed, returning the first responses
// and the leaks.
func selfTestSharedRun(t *testing.T, env *matrixEnv, seed int64) (map[string]caseResult, []string, []matrixCase) {
	t.Helper()
	m, probs := loadMatrix(selfTestFS(t), env.opts.modules)
	if len(probs) > 0 {
		t.Fatal(probs)
	}
	env.m = m
	groups := selfTestSharedGroups(t, env)
	first, leaks := map[string]caseResult{}, []string(nil)
	var cases []matrixCase
	for i, g := range groups {
		res := env.runShared(t, g, seed+int64(i))
		maps.Copy(first, res.first)
		leaks = append(leaks, res.leaks...)
		cases = append(append(cases, g.reads...), g.denials...)
	}
	return first, leaks, cases
}

// TestAuthzMatrix_SharedAppIsolation proves the shared application leaks no
// state between cases (BE-060): two runs in different shuffled orders give
// the same responses, and every response equals the one of a fresh
// application per case (the BE-004 behaviour), on selfTestSharedCases.
func TestAuthzMatrix_SharedAppIsolation(t *testing.T) {
	t.Parallel()
	env := selfTestEnv()
	r1, leaks1, cases := selfTestSharedRun(t, env, 1)
	r2, leaks2, _ := selfTestSharedRun(t, selfTestEnv(), 2)
	if len(leaks1)+len(leaks2) > 0 {
		t.Fatalf("leaks: %v %v", leaks1, leaks2)
	}
	if len(r1) != len(cases) || !maps.Equal(r1, r2) {
		t.Fatalf("shuffled runs differ:\n%v\n%v", r1, r2)
	}
	for _, c := range cases {
		got, body := env.runCase(t, c)
		if fresh := (caseResult{got, canonicalBody(body)}); fresh != r1[c.name] {
			t.Errorf("%s: shared application %v, fresh application %v", c.name, r1[c.name], fresh)
		}
	}
}

// TestAuthzMatrix_SharedAppExposesLeak proves the reread phase catches a
// write-then-deny bug: testOnlyCreateThing denies with the expected code but
// has already stored a thing, which testOnlyListThings then counts.
func TestAuthzMatrix_SharedAppExposesLeak(t *testing.T) {
	t.Parallel()
	_, leaks, _ := selfTestSharedRun(t, newSelfTestEnv(true), 1)
	if !slices.ContainsFunc(leaks, func(l string) bool {
		return strings.HasPrefix(l, "testOnlyListThings/") && strings.Contains(l, "state leaked on the shared application")
	}) {
		t.Fatalf("the leaky denial was not reported; leaks: %v", leaks)
	}
}

// TestAuthzMatrix_StatusOnlyDeclaration pins BE-061: an operation declaring
// compare: status is compared by status only on the shared application, for
// every variant and persona, whatever the interleaving of the reads; its
// status is still compared; the declaration is limited to reads and is
// required on every operation listing audit events.
func TestAuthzMatrix_StatusOnlyDeclaration(t *testing.T) {
	t.Parallel()
	f := caseResult{http.StatusOK, `{"count":1}`}
	grown := caseResult{http.StatusOK, `{"count":2}`}
	gone := caseResult{http.StatusNotFound, `{"count":1}`}
	plain, declared := matrixCase{name: "x/student"}, matrixCase{name: "x/student", statusOnly: true}
	for _, c := range []struct {
		name     string
		c        matrixCase
		r        caseResult
		unstable bool
		same     bool
	}{
		{"stable body differs", plain, grown, false, false},
		{"measured unstable body differs", plain, grown, true, true},
		{"declared body differs", declared, grown, false, true},
		{"declared status differs", declared, gone, false, false},
		{"plain status differs", plain, gone, true, false},
		{"identical", plain, f, false, true},
	} {
		if got := sameRead(c.c, f, c.r, c.unstable); got != c.same {
			t.Errorf("%s: sameRead = %v, want %v", c.name, got, c.same)
		}
	}

	// The real matrix declares every audit list and the event export list
	// (its download_count grows with every download read; BE-082), and only
	// them so far.
	m, probs := loadMatrix(os.DirFS("testdata/authz"), realMatrixOptions().modules)
	if len(probs) > 0 {
		t.Fatal(probs)
	}
	var got []string
	for id, o := range m.ops {
		if o.Compare == compareStatus {
			got = append(got, id)
		}
	}
	slices.Sort(got)
	if want := []string{"adminListAuditEvents", "listEventExports", "listMySecurityEvents", "listOrgAuditEvents"}; !slices.Equal(got, want) {
		t.Errorf("operations with compare: status = %v, want %v", got, want)
	}
	env := &matrixEnv{opts: realMatrixOptions(), m: m}
	for _, c := range env.cases() {
		id, _, _ := strings.Cut(c.name, "/")
		if c.statusOnly != slices.Contains(got, id) {
			t.Errorf("%s: statusOnly = %v", c.name, c.statusOnly)
		}
	}

	// Static checks on the self-test matrix.
	fsys := selfTestFS(t)
	declare := func(action string) fs.FS {
		return edit(t, fsys, "testonly.yaml", "    action: "+action+"\n", "    action: "+action+"\n    compare: status\n")
	}
	wantProblem(t, selfTestProblems(t, edit(t, fsys, "testonly.yaml", "    action: testonly.thing.list\n",
		"    action: testonly.thing.list\n    compare: body\n"), nil), `operation "testOnlyListThings": matrix compare may only be "status"`)
	wantProblem(t, selfTestProblems(t, declare("testonly.thing.create"), nil),
		`operation "testOnlyCreateThing": compare: status applies to reads (GET/HEAD) only`)
	asAuditList := func(a *matrixApp) {
		r := a.app.API.OpenAPI().Paths[httpx.APIPrefix+"/testonly/things"].Get.Responses["200"]
		r.Content["application/json"].Schema = &huma.Schema{Ref: "#/components/schemas/AuditEventList"}
	}
	wantProblem(t, selfTestProblems(t, fsys, asAuditList),
		`operation "testOnlyListThings" lists audit events (AuditEventList): declare compare: status`)
	if probs := selfTestProblems(t, declare("testonly.thing.list"), asAuditList); len(probs) > 0 {
		t.Errorf("declared audit list: %v", probs)
	}

	// On the shared application: the leaky denial of newSelfTestEnv(true)
	// grows testOnlyListThings (TestAuthzMatrix_SharedAppExposesLeak). Once
	// the list is declared status-only, no persona reports it, whatever the
	// seed; the other reads are still compared by body.
	for seed := int64(1); seed <= 3; seed++ {
		env := newSelfTestEnv(true)
		m, probs := loadMatrix(declare("testonly.thing.list"), env.opts.modules)
		if len(probs) > 0 {
			t.Fatal(probs)
		}
		env.m = m
		for i, g := range selfTestSharedGroups(t, env) {
			for _, l := range env.runShared(t, g, seed+int64(i)).leaks {
				t.Errorf("seed %d: %s", seed, l)
			}
		}
	}
}

// TestWire_PresetActorResolverSurvivesBuild pins the test ActorResolver
// connection of wire.go (R4): a preset resolver survives build, so every
// module and non-huma route sees the persona resolver in matrix tests.
func TestWire_PresetActorResolverSurvivesBuild(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	res := authztest.NewResolver(authztest.Personas()...)
	c.actors = res
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.actors != httpx.ActorResolver(res) {
		t.Fatal("build replaced the preset ActorResolver")
	}
}
