package main

import (
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

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
