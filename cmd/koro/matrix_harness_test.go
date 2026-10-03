package main

// Authz matrix harness (03 §7, 05 §13.5, BE-001 R3; BE-004). It merges
//
//	testdata/authz/_personas.yaml        18 personas, groups, default config (frozen after BE-004)
//	testdata/authz/_platform.yaml        platform routes outside OpenAPI (health)
//	testdata/authz/<module>.yaml         the operations of one module tag (+ its config keys,
//	                                     file-local groups, non-API routes, pending actions)
//	testdata/authz/fixtures/<module>.yaml named fixtures seeded by matrix_<module>_test.go
//	testdata/authz/scenarios/<module>.yaml multi-step scenarios (03 §7.7)
//	testdata/authz/bodies/<module>/*.json request bodies ("{{fixture:name}}" placeholders)
//
// and checks the 10 rules of 03 §7.4 against the OpenAPI spec, the gin
// routes and the authz action catalog, then runs every (operation, variant,
// persona) request (BE-060, see "driver" below):
//
//   - a case that may change state (a non-GET expected to succeed, 2xx or
//     an annotated 409/422) and every scenario runs on a fresh in-memory
//     application (05 §13.5);
//   - every other case (GET/HEAD, and non-GET denials 401/403/404) runs on
//     one shared application per config: first all reads, then all denials,
//     then all reads again in another shuffled order. The second read must
//     return exactly what the first returned (status and body), so a denial
//     that changed state, or a read with a side effect, fails the matrix.
//
// -matrix.fresh restores a fresh application for every case (the BE-004
// behaviour); -matrix.seed reproduces the shuffle of a run (it is logged).
//
// A module plugs in through optional methods on its <module>Wiring, written
// in cmd/koro/matrix_<module>_test.go (pattern of useMemStore):
//
//	func (w *eventsWiring) matrixFixtures(fx *matrixFixtures) error            // seed fixtures/<module>.yaml
//	func (w *talentWiring) matrixConfig(cfg *config.Modules, key string, v any) (bool, error) // own config keys
//	func (w *eventsWiring) matrixHook(fx *matrixFixtures, name string, args map[string]any) error // scenario hooks
//
// Rules for module tickets: never change an expected code to make the test
// pass (03 §7.1); a cell moving from 4xx to 2xx is justified in the PR.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"
	"go.yaml.in/yaml/v3"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/platform/config"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/obs"
)

// ---------------------------------------------------------------- module hooks

// matrixFixturer seeds the module's in-memory stores with its fixtures
// (fixtures/<module>.yaml) after build. IDs come from fx.ID (deterministic).
type matrixFixturer interface {
	matrixFixtures(fx *matrixFixtures) error
}

// matrixConfigurer applies a matrix config key owned by the module before
// build (e.g. talent_auditor_api_enabled → cfg.Talent). ok=false: not mine.
type matrixConfigurer interface {
	matrixConfig(cfg *config.Modules, key string, value any) (ok bool, err error)
}

// matrixHooker runs a scenario hook "<module>.<name>" (e.g. reset a
// company's entitlements) between scenario steps.
type matrixHooker interface {
	matrixHook(fx *matrixFixtures, name string, args map[string]any) error
}

// ---------------------------------------------------------------- YAML model

var allowedCodes = map[int]bool{200: true, 201: true, 202: true, 204: true, 401: true, 403: true, 404: true}

var (
	fixtureNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	pathParamRe   = regexp.MustCompile(`\{([a-zA-Z0-9_]+)\}`)
	placeholderRe = regexp.MustCompile(`\{\{fixture:([a-z][a-z0-9_]*)\}\}`)
)

const literalPrefix = "literal:"

type matrixFile struct {
	Version              int                    `yaml:"version"`
	Personas             []string               `yaml:"personas"`
	StrictActionCoverage *bool                  `yaml:"strict_action_coverage"`
	Groups               map[string][]string    `yaml:"groups"`
	Config               map[string]any         `yaml:"config"`
	Operations           map[string]*matrixOp   `yaml:"operations"`
	NonAPIRoutes         []*matrixRoute         `yaml:"non_api_routes"`
	PendingActions       []string               `yaml:"pending_actions"`
	Fixtures             map[string]fixtureSpec `yaml:"fixtures"`
	Scenarios            map[string]*scenario   `yaml:"scenarios"`
}

type matrixRequest struct {
	PathParams map[string]string `yaml:"path_params"`
	Query      map[string]string `yaml:"query"`
	Headers    map[string]string `yaml:"headers"`
	Body       string            `yaml:"body"`
	Config     map[string]any    `yaml:"config"`
}

type matrixOp struct {
	Method        string `yaml:"method"`
	Path          string `yaml:"path"`
	Action        string `yaml:"action"`
	Public        bool   `yaml:"public"`
	Idempotency   string `yaml:"idempotency"`
	matrixRequest `yaml:",inline"`
	Expect        map[string]int   `yaml:"expect"`
	Note          string           `yaml:"note"`
	Variants      []*matrixVariant `yaml:"variants"`

	file string // module file the operation is declared in
}

type matrixVariant struct {
	Name          string `yaml:"name"`
	matrixRequest `yaml:",inline"`
	Expect        map[string]int `yaml:"expect"`
	Note          string         `yaml:"note"`
}

type matrixRoute struct {
	Method     string            `yaml:"method"`
	Path       string            `yaml:"path"`
	PathParams map[string]string `yaml:"path_params"`
	Websocket  bool              `yaml:"websocket"`
	Expect     map[string]int    `yaml:"expect"`
	Note       string            `yaml:"note"`

	file string
}

type fixtureSpec map[string]any

type scenario struct {
	Note   string         `yaml:"note"`
	Config map[string]any `yaml:"config"`
	Steps  []*scenarioStep

	name, file string
}

type scenarioStep struct {
	Persona       string `yaml:"persona"`
	MFA           *bool  `yaml:"mfa"`
	Operation     string `yaml:"operation"`
	matrixRequest `yaml:",inline"`
	Expect        int               `yaml:"expect"`
	Code          string            `yaml:"code"`
	Save          map[string]string `yaml:"save"`
	Hook          string            `yaml:"hook"`
	Args          map[string]any    `yaml:"args"`
}

// matrix is the merged content of every file.
type matrix struct {
	fsys        fs.FS
	personas    []string
	groups      map[string][]string            // global (_personas.yaml)
	fileGroups  map[string]map[string][]string // file → local groups
	config      map[string]any                 // defaults
	configFile  map[string]string              // key → declaring file
	strict      bool
	ops         map[string]*matrixOp
	routes      []*matrixRoute
	moduleFiles map[string]bool     // modules with a <module>.yaml
	pending     map[string][]string // module → pending actions
	fixtures    map[string]fixture  // name → fixture
	fixtureMods map[string]bool     // modules with a fixtures/<module>.yaml
	scenarios   []*scenario
}

type fixture struct {
	Name, Module, Kind string
	Fields             map[string]any
}

// ---------------------------------------------------------------- loading

// loadMatrix reads and merges the matrix files of fsys. modules are the
// valid module file names. Problems are returned, not fatal, so the
// self-tests can assert them.
func loadMatrix(fsys fs.FS, modules []string) (*matrix, []string) {
	m := &matrix{
		fsys: fsys, groups: map[string][]string{}, fileGroups: map[string]map[string][]string{},
		config: map[string]any{}, configFile: map[string]string{}, ops: map[string]*matrixOp{},
		moduleFiles: map[string]bool{}, pending: map[string][]string{}, fixtures: map[string]fixture{},
		fixtureMods: map[string]bool{},
	}
	var probs []string
	bad := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }

	decode := func(name string) *matrixFile {
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			bad("%s: %v", name, err)
			return nil
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		var f matrixFile
		if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
			bad("%s: %v", name, err)
			return nil
		}
		if f.Version != 1 {
			bad("%s: version must be 1", name)
		}
		return &f
	}
	isModule := func(s string) bool { return slices.Contains(modules, s) }

	// _personas.yaml first: personas, groups, config defaults.
	pf := decode("_personas.yaml")
	if pf != nil {
		m.personas = pf.Personas
		m.groups = pf.Groups
		m.strict = pf.StrictActionCoverage != nil && *pf.StrictActionCoverage
		for k, v := range pf.Config {
			m.config[k], m.configFile[k] = v, "_personas.yaml"
		}
		if pf.Operations != nil || pf.NonAPIRoutes != nil || pf.PendingActions != nil || pf.Fixtures != nil || pf.Scenarios != nil {
			bad("_personas.yaml: only personas, groups, config and strict_action_coverage belong here")
		}
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		bad("read matrix dir: %v", err)
		return m, probs
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name != "fixtures" && name != "scenarios" && name != "bodies" {
				bad("unexpected directory %s/", name)
			}
			continue
		}
		mod, ok := strings.CutSuffix(name, ".yaml")
		switch {
		case !ok:
			bad("unexpected file %s (only .yaml)", name)
			continue
		case name == "_personas.yaml":
			continue
		case name == "_platform.yaml":
		case !isModule(mod):
			bad("%s: no such module (files are <module>.yaml, 01 §2)", name)
			continue
		}
		f := decode(name)
		if f == nil {
			continue
		}
		if f.Personas != nil || f.StrictActionCoverage != nil {
			bad("%s: personas live in _personas.yaml only", name)
		}
		if f.Fixtures != nil || f.Scenarios != nil {
			bad("%s: fixtures/scenarios belong in fixtures/%s and scenarios/%s", name, name, name)
		}
		if name == "_platform.yaml" && (f.Operations != nil || f.PendingActions != nil) {
			bad("_platform.yaml: only non_api_routes, groups and config belong here")
		}
		m.moduleFiles[mod] = name != "_platform.yaml"
		if len(f.Groups) > 0 {
			m.fileGroups[name] = f.Groups
		}
		for k, v := range f.Config {
			if prev, dup := m.configFile[k]; dup {
				bad("%s: config key %q is already declared in %s", name, k, prev)
				continue
			}
			m.config[k], m.configFile[k] = v, name
		}
		for id, o := range f.Operations {
			if o == nil {
				bad("%s: operation %q is empty", name, id)
				continue
			}
			if prev, dup := m.ops[id]; dup {
				bad("operation %q is declared in both %s and %s (one operationId, one file)", id, prev.file, name)
				continue
			}
			o.file = name
			m.ops[id] = o
		}
		for _, r := range f.NonAPIRoutes {
			r.file = name
			m.routes = append(m.routes, r)
		}
		if len(f.PendingActions) > 0 {
			m.pending[mod] = f.PendingActions
		}
	}

	// fixtures/<module>.yaml
	if fe, err := fs.ReadDir(fsys, "fixtures"); err == nil {
		core := coreFixtureNames()
		for _, e := range fe {
			mod, _ := strings.CutSuffix(e.Name(), ".yaml")
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") || !isModule(mod) {
				bad("fixtures/%s: files are fixtures/<module>.yaml", e.Name())
				continue
			}
			f := decode("fixtures/" + e.Name())
			if f == nil {
				continue
			}
			if f.Operations != nil || f.Personas != nil || f.Scenarios != nil || f.NonAPIRoutes != nil || f.Config != nil || f.Groups != nil {
				bad("fixtures/%s: only fixtures belong here", e.Name())
			}
			m.fixtureMods[mod] = true
			for _, n := range slices.Sorted(maps.Keys(f.Fixtures)) {
				spec := f.Fixtures[n]
				kind, _ := spec["kind"].(string)
				switch {
				case !fixtureNameRe.MatchString(n):
					bad("fixtures/%s: fixture name %q must be snake_case", e.Name(), n)
				case core[n]:
					bad("fixtures/%s: %q is a persona, helper user or org fixture of authztest", e.Name(), n)
				case kind == "":
					bad("fixtures/%s: fixture %q needs a kind", e.Name(), n)
				default:
					if prev, dup := m.fixtures[n]; dup {
						bad("fixture %q is declared in both fixtures/%s.yaml and fixtures/%s", n, prev.Module, e.Name())
						continue
					}
					fields := maps.Clone(map[string]any(spec))
					delete(fields, "kind")
					m.fixtures[n] = fixture{Name: n, Module: mod, Kind: kind, Fields: fields}
				}
			}
		}
	}

	// scenarios/<module>.yaml
	if se, err := fs.ReadDir(fsys, "scenarios"); err == nil {
		seen := map[string]string{}
		for _, e := range se {
			mod, _ := strings.CutSuffix(e.Name(), ".yaml")
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") || !isModule(mod) {
				bad("scenarios/%s: files are scenarios/<module>.yaml", e.Name())
				continue
			}
			f := decode("scenarios/" + e.Name())
			if f == nil {
				continue
			}
			if f.Operations != nil || f.Personas != nil || f.Fixtures != nil || f.NonAPIRoutes != nil || f.Config != nil || f.Groups != nil {
				bad("scenarios/%s: only scenarios belong here", e.Name())
			}
			for _, n := range slices.Sorted(maps.Keys(f.Scenarios)) {
				if prev, dup := seen[n]; dup {
					bad("scenario %q is declared in both %s and scenarios/%s", n, prev, e.Name())
					continue
				}
				seen[n] = "scenarios/" + e.Name()
				s := f.Scenarios[n]
				if s == nil {
					bad("scenarios/%s: scenario %q is empty", e.Name(), n)
					continue
				}
				s.name, s.file = n, "scenarios/"+e.Name()
				m.scenarios = append(m.scenarios, s)
			}
		}
	}
	return m, probs
}

// coreFixtureNames are the fixture names owned by authztest.
func coreFixtureNames() map[string]bool {
	out := map[string]bool{}
	for _, n := range append(slices.Clone(authztest.PersonaNames), authztest.HelperNames...) {
		out[n] = true
	}
	for _, o := range authztest.Orgs() {
		out[o.Name] = true
		if o.Kind == authz.OrgUniversity {
			out[o.Name+"_university"] = true
		}
	}
	return out
}

// ---------------------------------------------------------------- spec model

type specOp struct {
	ID, Method, Path, Tag, Action, Idempotency string
	Public                                     bool
}

// specOps walks the OpenAPI document (paths carry the /api/v1 prefix).
func specOps(api huma.API) []specOp {
	var out []specOp
	for p, pi := range api.OpenAPI().Paths {
		for method, o := range map[string]*huma.Operation{
			http.MethodGet: pi.Get, http.MethodPost: pi.Post, http.MethodPut: pi.Put, http.MethodPatch: pi.Patch,
			http.MethodDelete: pi.Delete, http.MethodHead: pi.Head, http.MethodOptions: pi.Options, http.MethodTrace: pi.Trace,
		} {
			if o == nil {
				continue
			}
			s := specOp{ID: o.OperationID, Method: method, Path: p, Public: len(o.Security) == 0}
			if len(o.Tags) > 0 {
				s.Tag = o.Tags[0]
			}
			s.Action, _ = o.Extensions[op.ExtAction].(string)
			s.Idempotency, _ = o.Extensions[op.ExtIdempotency].(string)
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b specOp) int { return strings.Compare(a.ID, b.ID) })
	return out
}

type routeKey struct{ Method, Path string }

// ginRoutes returns the engine routes with gin params as {name}.
func ginRoutes(e *gin.Engine) []routeKey {
	var out []routeKey
	for _, r := range e.Routes() {
		segs := strings.Split(r.Path, "/")
		for i, s := range segs {
			if len(s) > 1 && (s[0] == ':' || s[0] == '*') {
				segs[i] = "{" + s[1:] + "}"
			}
		}
		out = append(out, routeKey{r.Method, strings.Join(segs, "/")})
	}
	return out
}

// actionCatalog is what operations may bind (production: authz).
type actionCatalog struct {
	known    func(action string) bool // valid x-koro-action (HTTP actions only)
	coverage []string                 // actions that must be bound (03 §7.4-5)
}

func productionCatalog() actionCatalog {
	cov := make([]string, 0, len(authz.HTTPActions()))
	for _, a := range authz.HTTPActions() {
		cov = append(cov, string(a))
	}
	return actionCatalog{known: func(a string) bool { return authz.IsHTTPAction(authz.Action(a)) }, coverage: cov}
}

// ---------------------------------------------------------------- static checks

// checkInput is everything the static checks of 03 §7.4 (1–9) look at.
type checkInput struct {
	m        *matrix
	spec     []specOp
	routes   []routeKey
	catalog  actionCatalog
	seeders  map[string]bool // modules able to seed fixtures
	hookers  map[string]bool // modules able to run scenario hooks
	personas []string        // the persona list the matrix must use (authztest)
}

// checkMatrix returns the problems (test failures) and notes (logged).
func checkMatrix(in checkInput) (probs, notes []string) {
	m := in.m
	bad := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }

	// 8a. Personas: exactly the 18 of authztest, in report order, and every
	// one is defined (has a token or is anonymous).
	if !slices.Equal(m.personas, in.personas) {
		bad("_personas.yaml personas %v differ from the 18 personas of 03 §1.1 + IDOR %v", m.personas, in.personas)
	}
	for _, scope := range append([]map[string][]string{m.groups}, slices.Collect(maps.Values(m.fileGroups))...) {
		for g, members := range scope {
			if g == "all" {
				bad("group name @all is reserved")
			}
			for _, p := range members {
				if !slices.Contains(m.personas, p) {
					bad("group %s: unknown persona %q", g, p)
				}
			}
		}
	}
	for f, local := range m.fileGroups {
		for g := range local {
			if _, clash := m.groups[g]; clash {
				bad("%s: local group %s shadows a _personas.yaml group", f, g)
			}
		}
	}

	specByID := map[string]specOp{}
	bound := map[string]bool{}
	for _, s := range in.spec {
		specByID[s.ID] = s
		if s.Action != "" {
			bound[s.Action] = true
		}
		e, ok := m.ops[s.ID]
		// 1. spec → matrix
		if !ok {
			bad("operation %q (%s %s, tag %s) is missing from the authz matrix: add it to testdata/authz/%s.yaml",
				s.ID, s.Method, s.Path, s.Tag, s.Tag)
			continue
		}
		if file := strings.TrimSuffix(e.file, ".yaml"); file != s.Tag {
			bad("operation %q has tag %q but is in %s (one file per module tag, R3)", s.ID, s.Tag, e.file)
		}
		// 3. method/path
		if !strings.EqualFold(e.Method, s.Method) || e.Path != s.Path {
			bad("operation %q: matrix says %s %s, spec says %s %s", s.ID, e.Method, e.Path, s.Method, s.Path)
		}
		// 4. action
		switch {
		case s.Action == "":
			bad("operation %q has no x-koro-action", s.ID)
		case s.Action != e.Action:
			bad("operation %q: x-koro-action %q, matrix action %q", s.ID, s.Action, e.Action)
		case !in.catalog.known(s.Action):
			bad("operation %q: action %q is not an HTTP action of authz (03 §5)", s.ID, s.Action)
		}
		// 6. public ⇔ empty Security
		if s.Public != e.Public {
			bad("operation %q: empty Security=%v but matrix public=%v", s.ID, s.Public, e.Public)
		}
		// 7. idempotency
		if (s.Idempotency == "required") != (e.Idempotency == "required") {
			bad("operation %q: x-koro-idempotency %q but matrix idempotency %q", s.ID, s.Idempotency, e.Idempotency)
		}
		if e.Idempotency != "" && e.Idempotency != "required" {
			bad("operation %q: matrix idempotency may only be \"required\"", s.ID)
		}
		checkOpCells(in, s, e, bad)
	}
	// 2. matrix → spec
	for _, id := range slices.Sorted(maps.Keys(m.ops)) {
		if _, ok := specByID[id]; !ok {
			bad("stale matrix entry %q (%s): not in the OpenAPI spec", id, m.ops[id].file)
		}
	}

	// 5. action coverage (no dead action), per module file + pending_actions.
	var unboundNoFile []string
	for _, a := range in.catalog.coverage {
		if bound[a] {
			continue
		}
		mod, _, _ := strings.Cut(a, ".")
		switch {
		case slices.Contains(m.pending[mod], a):
		case m.moduleFiles[mod] || m.strict:
			bad("action %s is bound to no operation (03 §7.4-5): bind it, or list it under pending_actions in %s.yaml with its ticket", a, mod)
		default:
			unboundNoFile = append(unboundNoFile, a)
		}
	}
	for mod, list := range m.pending {
		seen := map[string]bool{}
		for _, a := range list {
			am, _, _ := strings.Cut(a, ".")
			switch {
			case seen[a]:
				bad("%s.yaml: pending action %s listed twice", mod, a)
			case am != mod || !slices.Contains(in.catalog.coverage, a):
				bad("%s.yaml: pending action %s is not an HTTP action of %s", mod, a, mod)
			case bound[a]:
				bad("%s.yaml: pending action %s is bound now: remove it from pending_actions", mod, a)
			}
			seen[a] = true
		}
	}
	if len(unboundNoFile) > 0 {
		notes = append(notes, fmt.Sprintf("%d HTTP actions of modules without a matrix file are not bound yet "+
			"(strict_action_coverage=false)", len(unboundNoFile)))
	}

	// 9. gin routes ⇔ huma operations + non_api_routes.
	specRoutes := map[routeKey]bool{}
	for _, s := range in.spec {
		specRoutes[routeKey{s.Method, s.Path}] = true
	}
	listed := map[routeKey]bool{}
	for _, r := range m.routes {
		k := routeKey{strings.ToUpper(r.Method), r.Path}
		if listed[k] {
			bad("non_api_routes: %s %s listed twice", k.Method, k.Path)
		}
		listed[k] = true
		checkRouteCells(in, r, bad)
	}
	engine := map[routeKey]bool{}
	for _, r := range in.routes {
		engine[r] = true
		if !specRoutes[r] && !listed[r] {
			bad("gin route %s %s is neither a huma operation nor in non_api_routes", r.Method, r.Path)
		}
	}
	for k := range listed {
		if !engine[k] {
			bad("stale non_api_routes entry %s %s: no such gin route", k.Method, k.Path)
		}
	}

	// Fixtures: every fixture file has a seeder.
	for mod := range m.fixtureMods {
		if !in.seeders[mod] {
			bad("fixtures/%s.yaml has no seeder: implement matrixFixtures in cmd/koro/matrix_%s_test.go", mod, mod)
		}
	}
	checkScenarios(in, specByID, bad)
	slices.Sort(probs)
	return probs, notes
}

// checkOpCells validates expectations, variants, params, bodies and config.
func checkOpCells(in checkInput, s specOp, e *matrixOp, bad func(string, ...any)) {
	where := "operation " + strconv.Quote(s.ID)
	checkExpect(in, where, e.file, e.Expect, s.Public, e.Note, s.Method, bad)
	checkRequest(in, where, s.Path, e.matrixRequest, e.PathParams, bad)
	names := map[string]bool{}
	for _, v := range e.Variants {
		vw := where + " variant " + strconv.Quote(v.Name)
		if !fixtureNameRe.MatchString(v.Name) || names[v.Name] {
			bad("%s: variant names are unique snake_case", vw)
		}
		names[v.Name] = true
		params := maps.Clone(e.PathParams)
		if params == nil {
			params = map[string]string{}
		}
		maps.Copy(params, v.PathParams)
		checkExpect(in, vw, e.file, v.Expect, s.Public, v.Note+e.Note, s.Method, bad)
		checkRequest(in, vw, s.Path, v.matrixRequest, params, bad)
	}
}

func checkRouteCells(in checkInput, r *matrixRoute, bad func(string, ...any)) {
	where := "non_api_route " + r.Method + " " + r.Path
	checkExpect(in, where, r.file, r.Expect, true, r.Note, http.MethodGet, bad)
	checkRequest(in, where, r.Path, matrixRequest{PathParams: r.PathParams}, r.PathParams, bad)
	for p, c := range r.Expect {
		if c == http.StatusSwitchingProtocols && !r.Websocket {
			bad("%s: %s expects 101 but the route is not websocket: true", where, p)
		}
	}
}

// checkExpect is 03 §7.1: after expansion every persona exactly once;
// allowed codes; 409/422 and anonymous writes need a note; @all only on
// public operations and non-API routes.
func checkExpect(in checkInput, where, file string, expect map[string]int, public bool, note, method string, bad func(string, ...any)) {
	if _, err := expandExpect(in.m, file, expect, public); err != nil {
		bad("%s: %v", where, err)
	}
	for k, c := range expect {
		switch {
		case allowedCodes[c], c == http.StatusSwitchingProtocols:
		case (c == http.StatusConflict || c == http.StatusUnprocessableEntity) && note != "":
		case c == http.StatusConflict || c == http.StatusUnprocessableEntity:
			bad("%s: %s expects %d without a note (03 §7.1)", where, k, c)
		default:
			bad("%s: %s expects %d (allowed: 200 201 202 204 401 403 404; 409/422 with note)", where, k, c)
		}
	}
	if public && method != http.MethodGet && method != http.MethodHead && note == "" {
		bad("%s: anonymous write operation needs a note with its justification (05 §13.5-b)", where)
	}
}

// expandExpect resolves @group keys; every persona must appear exactly once.
func expandExpect(m *matrix, file string, expect map[string]int, allowAll bool) (map[string]int, error) {
	out := map[string]int{}
	var errs []string
	add := func(p string, c int, via string) {
		if !slices.Contains(m.personas, p) {
			errs = append(errs, fmt.Sprintf("unknown persona %q%s", p, via))
			return
		}
		if _, dup := out[p]; dup {
			errs = append(errs, fmt.Sprintf("persona %s appears more than once%s", p, via))
			return
		}
		out[p] = c
	}
	for _, k := range slices.Sorted(maps.Keys(expect)) {
		c := expect[k]
		g, isGroup := strings.CutPrefix(k, "@")
		switch {
		case !isGroup:
			add(k, c, "")
		case g == "all":
			if !allowAll {
				errs = append(errs, "@all is allowed only on public operations and non_api_routes")
				continue
			}
			for _, p := range m.personas {
				add(p, c, " (via @all)")
			}
		default:
			members, ok := m.groups[g]
			if !ok {
				members, ok = m.fileGroups[file][g]
			}
			if !ok {
				errs = append(errs, "unknown group @"+g)
				continue
			}
			for _, p := range members {
				add(p, c, " (via @"+g+")")
			}
		}
	}
	for _, p := range m.personas {
		if _, ok := out[p]; !ok {
			errs = append(errs, "no expectation for persona "+p)
		}
	}
	if len(errs) > 0 {
		return out, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

// checkRequest validates path params against the template, fixture names,
// bodies and config keys.
func checkRequest(in checkInput, where, tmpl string, r matrixRequest, params map[string]string, bad func(string, ...any)) {
	m := in.m
	want := map[string]bool{}
	for _, mm := range pathParamRe.FindAllStringSubmatch(tmpl, -1) {
		want[mm[1]] = true
		if _, ok := params[mm[1]]; !ok {
			bad("%s: path parameter %s has no path_params entry", where, mm[1])
		}
	}
	for p, v := range params {
		if !want[p] {
			bad("%s: path_params.%s is not a parameter of %s", where, p, tmpl)
		}
		if !strings.HasPrefix(v, literalPrefix) && !knownFixture(m, v) {
			bad("%s: path_params.%s: unknown fixture %q", where, p, v)
		}
	}
	for _, v := range append(slices.Collect(maps.Values(r.Query)), slices.Collect(maps.Values(r.Headers))...) {
		for _, mm := range placeholderRe.FindAllStringSubmatch(v, -1) {
			if !knownFixture(m, mm[1]) {
				bad("%s: unknown fixture %q in a placeholder", where, mm[1])
			}
		}
	}
	if r.Body != "" {
		b, err := fs.ReadFile(m.fsys, r.Body)
		switch {
		case err != nil || !strings.HasPrefix(r.Body, "bodies/"):
			bad("%s: body %s: must be a file under bodies/ (%v)", where, r.Body, err)
		default:
			for _, mm := range placeholderRe.FindAllStringSubmatch(string(b), -1) {
				if !knownFixture(m, mm[1]) {
					bad("%s: body %s: unknown fixture %q", where, r.Body, mm[1])
				}
			}
		}
	}
	for k := range r.Config {
		if _, ok := m.config[k]; !ok {
			bad("%s: config key %q has no default (declare it under config: in its module file)", where, k)
		}
	}
}

func knownFixture(m *matrix, name string) bool {
	if name == "anonymous" {
		return false
	}
	if coreFixtureNames()[name] {
		return true
	}
	_, ok := m.fixtures[name]
	return ok
}

func checkScenarios(in checkInput, specByID map[string]specOp, bad func(string, ...any)) {
	m := in.m
	for _, s := range m.scenarios {
		where := s.file + ": scenario " + s.name
		if len(s.Steps) == 0 {
			bad("%s: no steps", where)
		}
		saved := map[string]bool{}
		pos, neg := false, false
		for i, st := range s.Steps {
			sw := fmt.Sprintf("%s step %d", where, i+1)
			if st.Hook != "" {
				mod, _, ok := strings.Cut(st.Hook, ".")
				if !ok || !in.hookers[mod] {
					bad("%s: hook %q: no matrixHook for module %s", sw, st.Hook, mod)
				}
				if st.Operation != "" || st.Persona != "" || st.Expect != 0 {
					bad("%s: a hook step has no request fields", sw)
				}
				continue
			}
			o, ok := m.ops[st.Operation]
			sp, inSpec := specByID[st.Operation]
			if !ok || !inSpec {
				bad("%s: operation %q is not in the matrix and the spec", sw, st.Operation)
				continue
			}
			if _, ok := authztest.Get(st.Persona); !ok {
				bad("%s: unknown persona or helper %q", sw, st.Persona)
			}
			switch {
			case st.Expect >= 200 && st.Expect < 300:
				pos = true
			case st.Expect >= 400 && st.Expect < 500:
				neg = true
			default:
				bad("%s: expect %d", sw, st.Expect)
			}
			params := maps.Clone(o.PathParams)
			if params == nil {
				params = map[string]string{}
			}
			maps.Copy(params, st.PathParams)
			for p, v := range params {
				if saved[v] {
					params[p] = literalPrefix + v // resolved at run time
				}
			}
			checkRequest(in, sw, sp.Path, st.matrixRequest, params, bad)
			if st.Config != nil {
				bad("%s: config belongs to the scenario, not to a step", sw)
			}
			for name := range st.Save {
				if !fixtureNameRe.MatchString(name) || knownFixture(m, name) {
					bad("%s: save name %q must be a new snake_case name", sw, name)
				}
				saved[name] = true
			}
		}
		for k := range s.Config {
			if _, ok := m.config[k]; !ok {
				bad("%s: config key %q has no default", where, k)
			}
		}
		if !pos || !neg {
			bad("%s: needs at least one positive (2xx) and one negative (4xx) step (03 §7.7)", where)
		}
	}
}

// ---------------------------------------------------------------- application

// matrixFixtures resolves fixture names to IDs and hands modules their
// fixtures. Scenario steps may save response IDs under new names.
type matrixFixtures struct {
	m     *matrix
	mu    sync.Mutex
	saved map[string]uuid.UUID
}

// ID returns the ID of a persona, helper user, org fixture or declared
// fixture (deterministic), or of a name saved by a scenario step.
func (fx *matrixFixtures) ID(name string) (uuid.UUID, error) {
	fx.mu.Lock()
	id, ok := fx.saved[name]
	fx.mu.Unlock()
	if ok {
		return id, nil
	}
	if !knownFixture(fx.m, name) {
		return uuid.Nil(), fmt.Errorf("unknown fixture %q", name)
	}
	return authztest.ID(name), nil
}

// Of returns the fixtures of one module, sorted by name.
func (fx *matrixFixtures) Of(module string) []fixture {
	var out []fixture
	for _, n := range slices.Sorted(maps.Keys(fx.m.fixtures)) {
		if f := fx.m.fixtures[n]; f.Module == module {
			out = append(out, f)
		}
	}
	return out
}

// Ref resolves a fixture field holding another fixture's name.
func (fx *matrixFixtures) Ref(f fixture, field string) (uuid.UUID, error) {
	name, ok := f.Fields[field].(string)
	if !ok {
		return uuid.Nil(), fmt.Errorf("fixture %s: field %s must name a fixture", f.Name, field)
	}
	return fx.ID(name)
}

func (fx *matrixFixtures) save(name string, id uuid.UUID) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.saved[name] = id
}

// matrixOptions customise the harness (the self-test registers a test-only
// module instead of the real routes).
type matrixOptions struct {
	modules    []string                            // valid module file names
	catalog    actionCatalog                       // operations' actions
	routes     func(a *matrixApp)                  // nil → c.registerRoutes
	fixtures   map[string]func(a *matrixApp) error // extra seeders by module
	hooks      map[string]func(a *matrixApp, name string, args map[string]any) error
	configKeys map[string]bool // keys the options apply themselves (read a.cfg)
}

// matrixEnv is one matrix directory plus its options.
type matrixEnv struct {
	m    *matrix
	opts matrixOptions
}

type matrixApp struct {
	c   *container
	app *httpx.App
	res *authztest.Resolver
	fx  *matrixFixtures
	cfg map[string]any
}

func realMatrixOptions() matrixOptions {
	return matrixOptions{modules: authz.Modules(), catalog: productionCatalog()}
}

// newMatrixApp builds a fresh application: in-memory stores, the persona
// resolver, cfg applied before build, fixtures seeded after build. A case
// that may change state gets its own (05 §13.5); the others share one per
// config and prove they left no state behind (runShared, BE-060).
func (env *matrixEnv) newMatrixApp(t *testing.T, cfg map[string]any) *matrixApp {
	t.Helper()
	c := newTestContainer(t)
	a := &matrixApp{c: c, fx: &matrixFixtures{m: env.m, saved: map[string]uuid.UUID{}}, cfg: cfg}
	for _, k := range slices.Sorted(maps.Keys(cfg)) {
		claimed := env.opts.configKeys[k]
		for _, mod := range c.modules() {
			if mc, ok := mod.w.(matrixConfigurer); ok {
				ok, err := mc.matrixConfig(&c.infra.cfg, k, cfg[k])
				if err != nil {
					t.Fatalf("config %s: %v", k, err)
				}
				claimed = claimed || ok
			}
		}
		if !claimed && !reflect.DeepEqual(cfg[k], env.m.config[k]) {
			t.Fatalf("config %s=%v: no module applies it (implement matrixConfig in its matrix_<module>_test.go)", k, cfg[k])
		}
	}
	a.res = authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)
	c.actors = a.res
	if err := c.build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, mod := range c.modules() {
		if f, ok := mod.w.(matrixFixturer); ok {
			if err := f.matrixFixtures(a.fx); err != nil {
				t.Fatalf("fixtures of %s: %v", mod.name, err)
			}
		}
	}
	for _, mod := range slices.Sorted(maps.Keys(env.opts.fixtures)) {
		if err := env.opts.fixtures[mod](a); err != nil {
			t.Fatalf("fixtures of %s: %v", mod, err)
		}
	}
	checkMembershipsHook(t, c)

	ready := httpx.NewReady(c.readyChecks...)
	ready.Set(true)
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Version: apiVersion, Actors: c.actors, Ready: ready})
	if err != nil {
		t.Fatal(err)
	}
	a.app = app
	if env.opts.routes != nil {
		env.opts.routes(a)
	} else {
		c.registerRoutes(app.V1)
	}
	return a
}

// checkMembershipsHook keeps the orgs fixtures and the personas in step:
// once orgs fills hooks.memberships, it must return exactly the
// memberships of every persona and helper (03 §7.3).
func checkMembershipsHook(t *testing.T, c *container) {
	t.Helper()
	if c.hooks.memberships == nil {
		return
	}
	key := func(ms []authz.Membership) []string {
		out := make([]string, 0, len(ms))
		for _, m := range ms {
			out = append(out, m.OrgID.String()+"/"+string(m.OrgKind)+"/"+string(m.Role))
		}
		slices.Sort(out)
		return out
	}
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if !p.Actor.IsUser() {
			continue
		}
		got, err := c.hooks.memberships(t.Context(), p.User())
		if err != nil {
			t.Fatalf("hooks.memberships(%s): %v", p.Name, err)
		}
		if !slices.Equal(key(got), key(p.Actor.Memberships)) {
			t.Fatalf("hooks.memberships(%s) = %v, authztest persona has %v: seed org_members from authztest in matrix_orgs_test.go",
				p.Name, key(got), key(p.Actor.Memberships))
		}
	}
}

// specAndRoutes builds one application to read the spec and gin routes.
func (env *matrixEnv) specAndRoutes(t *testing.T) ([]specOp, []routeKey) {
	t.Helper()
	a := env.newMatrixApp(t, env.m.config)
	return specOps(a.app.API), ginRoutes(a.app.Engine)
}

func (env *matrixEnv) checkInput(spec []specOp, routes []routeKey) checkInput {
	seeders, hookers := map[string]bool{}, map[string]bool{}
	for _, mod := range newContainer(infra{}).modules() {
		_, seeders[mod.name] = mod.w.(matrixFixturer)
		_, hookers[mod.name] = mod.w.(matrixHooker)
	}
	for mod := range env.opts.fixtures {
		seeders[mod] = true
	}
	for mod := range env.opts.hooks {
		hookers[mod] = true
	}
	return checkInput{m: env.m, spec: spec, routes: routes, catalog: env.opts.catalog,
		seeders: seeders, hookers: hookers, personas: authztest.PersonaNames}
}

// ---------------------------------------------------------------- requests

type matrixCase struct {
	name      string
	method    string
	path      string // template
	req       matrixRequest
	config    map[string]any
	idem      bool
	websocket bool
	persona   string
	mfa       *bool
	want      int
}

// cases expands every operation, variant and non-API route per persona.
func (env *matrixEnv) cases() []matrixCase {
	m := env.m
	var out []matrixCase
	for _, id := range slices.Sorted(maps.Keys(m.ops)) {
		o := m.ops[id]
		base := matrixCase{method: strings.ToUpper(o.Method), path: o.Path, req: o.matrixRequest, idem: o.Idempotency == "required"}
		runs := []struct {
			name   string
			req    matrixRequest
			expect map[string]int
		}{{id, o.matrixRequest, o.Expect}}
		for _, v := range o.Variants {
			runs = append(runs, struct {
				name   string
				req    matrixRequest
				expect map[string]int
			}{id + "/" + v.Name, mergeRequest(o.matrixRequest, v.matrixRequest), v.Expect})
		}
		for _, r := range runs {
			exp, _ := expandExpect(m, o.file, r.expect, o.Public)
			for _, p := range m.personas {
				c := base
				c.name, c.req, c.persona, c.want = r.name+"/"+p, r.req, p, exp[p]
				c.config = mergeConfig(m.config, r.req.Config)
				out = append(out, c)
			}
		}
	}
	for _, r := range m.routes {
		exp, _ := expandExpect(m, r.file, r.Expect, true)
		for _, p := range m.personas {
			out = append(out, matrixCase{
				name: "route " + r.Method + " " + r.Path + "/" + p, method: strings.ToUpper(r.Method), path: r.Path,
				req: matrixRequest{PathParams: r.PathParams}, config: m.config, websocket: r.Websocket, persona: p, want: exp[p],
			})
		}
	}
	return out
}

func mergeRequest(base, v matrixRequest) matrixRequest {
	out := base
	merge := func(a, b map[string]string) map[string]string {
		if len(b) == 0 {
			return a
		}
		o := maps.Clone(a)
		if o == nil {
			o = map[string]string{}
		}
		maps.Copy(o, b)
		return o
	}
	out.PathParams, out.Query, out.Headers = merge(base.PathParams, v.PathParams), merge(base.Query, v.Query), merge(base.Headers, v.Headers)
	if v.Body != "" {
		out.Body = v.Body
	}
	out.Config = maps.Clone(base.Config)
	if out.Config == nil {
		out.Config = map[string]any{}
	}
	maps.Copy(out.Config, v.Config)
	return out
}

func mergeConfig(defaults map[string]any, over map[string]any) map[string]any {
	out := maps.Clone(defaults)
	maps.Copy(out, over)
	return out
}

// runCase executes one case on a fresh application and returns the status
// and the response body.
func (env *matrixEnv) runCase(t *testing.T, c matrixCase) (int, string) {
	t.Helper()
	a := env.newMatrixApp(t, c.config)
	return a.do(t, env.m, c)
}

// do sends one request as a persona on an existing application.
func (a *matrixApp) do(t *testing.T, m *matrix, c matrixCase) (int, string) {
	t.Helper()
	resolve := func(v string) string {
		if lit, ok := strings.CutPrefix(v, literalPrefix); ok {
			return lit
		}
		id, err := a.fx.ID(v)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		return id.String()
	}
	fill := func(s string) string {
		return placeholderRe.ReplaceAllStringFunc(s, func(ph string) string {
			return resolve(placeholderRe.FindStringSubmatch(ph)[1])
		})
	}
	p := pathParamRe.ReplaceAllStringFunc(c.path, func(seg string) string {
		return url.PathEscape(resolve(c.req.PathParams[seg[1:len(seg)-1]]))
	})
	if len(c.req.Query) > 0 {
		q := url.Values{}
		for k, v := range c.req.Query {
			q.Set(k, fill(v))
		}
		p += "?" + q.Encode()
	}
	var body io.Reader
	if c.req.Body != "" {
		b, err := fs.ReadFile(m.fsys, c.req.Body)
		if err != nil {
			t.Fatal(err)
		}
		body = strings.NewReader(fill(string(b)))
	}
	hdr := http.Header{}
	for k, v := range c.req.Headers {
		hdr.Set(k, fill(v))
	}
	if body != nil {
		hdr.Set("Content-Type", "application/json")
	}
	if hdr.Get(httpx.RequestIDHeader) == "" {
		// Deterministic per case, so problem bodies (instance) of two runs
		// of the same case compare equal on the shared application.
		sum := sha256.Sum256([]byte(c.name))
		hdr.Set(httpx.RequestIDHeader, "matrix-"+hex.EncodeToString(sum[:8]))
	}
	if c.idem {
		hdr.Set("Idempotency-Key", uuid.NewV7().String())
	}
	if tok := a.token(t, c.persona, c.mfa); tok != "" {
		hdr.Set("Authorization", "Bearer "+tok)
	}

	if c.websocket {
		srv := httptest.NewServer(a.app.Handler())
		defer srv.Close()
		req, err := http.NewRequestWithContext(t.Context(), c.method, srv.URL+p, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header = hdr
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, string(b)
	}
	req := httptest.NewRequestWithContext(t.Context(), c.method, p, body)
	req.Header = hdr
	rec := httptest.NewRecorder()
	a.app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// token returns the bearer token of a persona or helper ("" for
// anonymous); mfa overrides the session's MFA state (scenario steps).
func (a *matrixApp) token(t *testing.T, name string, mfa *bool) string {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %q", name)
	}
	if mfa == nil || p.Token == "" || *mfa == p.Actor.MFAVerified {
		return p.Token
	}
	actor := p.Actor
	actor.MFAVerified = *mfa
	tok := fmt.Sprintf("%s#mfa=%v", p.Token, *mfa)
	a.res.Add(tok, actor)
	return tok
}

// ---------------------------------------------------------------- scenarios

// runScenario runs the steps of one scenario on a single application.
func (env *matrixEnv) runScenario(t *testing.T, s *scenario) {
	t.Helper()
	a := env.newMatrixApp(t, mergeConfig(env.m.config, s.Config))
	for i, st := range s.Steps {
		if st.Hook != "" {
			mod, name, _ := strings.Cut(st.Hook, ".")
			var err error
			if h, ok := env.opts.hooks[mod]; ok {
				err = h(a, name, st.Args)
			} else {
				for _, m := range a.c.modules() {
					if hk, ok := m.w.(matrixHooker); ok && m.name == mod {
						err = hk.matrixHook(a.fx, name, st.Args)
					}
				}
			}
			if err != nil {
				t.Fatalf("step %d: hook %s: %v", i+1, st.Hook, err)
			}
			continue
		}
		o := env.m.ops[st.Operation]
		c := matrixCase{
			name: fmt.Sprintf("%s step %d", s.name, i+1), method: strings.ToUpper(o.Method), path: o.Path,
			req: mergeRequest(o.matrixRequest, st.matrixRequest), idem: o.Idempotency == "required",
			persona: st.Persona, mfa: st.MFA, want: st.Expect,
		}
		got, body := a.do(t, env.m, c)
		if got != st.Expect {
			t.Fatalf("step %d (%s as %s): got %d, want %d (body: %s)", i+1, st.Operation, st.Persona, got, st.Expect, body)
		}
		if st.Code != "" {
			var pb struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal([]byte(body), &pb); err != nil || pb.Code != st.Code {
				t.Fatalf("step %d (%s as %s): code %q, want %q (body: %s)", i+1, st.Operation, st.Persona, pb.Code, st.Code, body)
			}
		}
		for name, field := range st.Save {
			var obj map[string]any
			if err := json.Unmarshal([]byte(body), &obj); err != nil {
				t.Fatalf("step %d: save %s: %v", i+1, name, err)
			}
			s, _ := obj[field].(string)
			id, err := uuid.Parse(s)
			if err != nil {
				t.Fatalf("step %d: save %s: response field %q is not a UUID (%s)", i+1, name, field, body)
			}
			a.fx.save(name, id)
		}
	}
}

// ---------------------------------------------------------------- driver

// run is TestAuthzMatrix for one matrix directory: load, static checks,
// then every case and scenario as a parallel subtest.
func (env *matrixEnv) run(t *testing.T, fsys fs.FS) {
	t.Helper()
	m, probs := loadMatrix(fsys, env.opts.modules)
	env.m = m
	for _, p := range probs {
		t.Error(p)
	}
	if len(probs) > 0 {
		return
	}
	spec, routes := env.specAndRoutes(t)
	probs, notes := checkMatrix(env.checkInput(spec, routes))
	for _, n := range notes {
		t.Log(n)
	}
	for _, p := range probs {
		t.Error(p)
	}
	if len(probs) > 0 {
		return
	}
	all := env.cases()
	groups, fresh := partitionCases(all, *matrixFresh)
	seed := *matrixSeed
	if seed == 0 {
		seed = rand.Int64N(1<<62) + 1
	}
	t.Logf("%d cases + %d scenarios: %d on %d shared applications, %d on fresh applications (-matrix.seed=%d)",
		len(all), len(m.scenarios), len(all)-len(fresh), len(groups), len(fresh), seed)

	for _, c := range fresh {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got, body := env.runCase(t, c); got != c.want {
				t.Errorf("%s %s as %s: got %d, want %d (body: %s)", c.method, c.path, c.persona, got, c.want, body)
			}
		})
	}
	for i, g := range groups {
		t.Run(fmt.Sprintf("shared%d", i+1), func(t *testing.T) {
			t.Parallel()
			t.Logf("config %s: %d reads, %d denials", g.key, len(g.reads), len(g.denials))
			res := env.runShared(t, g, seed+int64(i))
			for _, l := range res.leaks {
				t.Error(l)
			}
		})
	}
	for _, s := range m.scenarios {
		t.Run("scenario/"+s.name, func(t *testing.T) {
			t.Parallel()
			env.runScenario(t, s)
		})
	}
}

var (
	matrixFresh = flag.Bool("matrix.fresh", false,
		"authz matrix: run every case on a fresh application (no shared application; BE-004 behaviour)")
	matrixSeed = flag.Int64("matrix.seed", 0, "authz matrix: shuffle seed of the shared applications (0: random, logged)")
)

// sharedSafe reports whether a case may run on the shared application of
// its config: a read (GET/HEAD, WebSocket upgrade included) or a request
// expected to be denied (401 by the auth middleware, 403/404 by the
// visibility and authz checks that precede any write). A request expected
// to succeed, or to fail later in the service (409/422), may change state.
func (c matrixCase) sharedSafe() bool {
	if c.read() {
		return true
	}
	return c.want == http.StatusUnauthorized || c.want == http.StatusForbidden || c.want == http.StatusNotFound
}

func (c matrixCase) read() bool { return c.method == http.MethodGet || c.method == http.MethodHead }

// sharedGroup is the cases of one config that run on one shared application.
type sharedGroup struct {
	key            string // canonical config
	config         map[string]any
	reads, denials []matrixCase
}

// partitionCases splits the cases into shared groups (one per distinct
// config, in a stable order) and the cases needing a fresh application.
// Every case lands in exactly one place (TestAuthzMatrix_CasePartition).
func partitionCases(cases []matrixCase, allFresh bool) (groups []*sharedGroup, fresh []matrixCase) {
	byKey := map[string]*sharedGroup{}
	for _, c := range cases {
		if allFresh || !c.sharedSafe() {
			fresh = append(fresh, c)
			continue
		}
		k := configKey(c.config)
		g := byKey[k]
		if g == nil {
			g = &sharedGroup{key: k, config: c.config}
			byKey[k] = g
			groups = append(groups, g)
		}
		if c.read() {
			g.reads = append(g.reads, c)
		} else {
			g.denials = append(g.denials, c)
		}
	}
	slices.SortFunc(groups, func(a, b *sharedGroup) int { return strings.Compare(a.key, b.key) })
	return groups, fresh
}

func configKey(cfg map[string]any) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(cfg)) {
		fmt.Fprintf(&b, "%s=%v;", k, cfg[k])
	}
	return b.String()
}

// caseResult is a response as compared between two runs of a read.
type caseResult struct {
	status int
	body   string // canonical (see canonicalBody)
}

type sharedRun struct {
	first    map[string]caseResult // first run of every read and denial, by case name
	unstable map[string]bool       // reads whose body differs when run again before any denial
	leaks    []string              // reads whose last run differs from the first
}

// runShared runs one group on a single application in four phases, each a
// set of parallel subtests (t.Run returns when they are done):
//
//  1. read: every read, shuffled; the responses are recorded;
//  2. recheck: every read again in the same order, so every other read
//     runs between a read's two runs. A read whose body changes here
//     depends on time or on the side effects of reads (the audit trail of
//     audited reads): its body is not compared later, its status is. A
//     changed status is a leak already;
//  3. deny: every denial, shuffled;
//  4. reread: every read in another order. Its status, and its body unless
//     unstable, must equal the first run: no denial and no read left state
//     behind that another case can see.
func (env *matrixEnv) runShared(t *testing.T, g *sharedGroup, seed int64) sharedRun {
	t.Helper()
	a := env.newMatrixApp(t, g.config)
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)>>1|1))
	shuffled := func(cs []matrixCase) []matrixCase {
		out := slices.Clone(cs)
		rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}
	var mu sync.Mutex
	run := sharedRun{first: map[string]caseResult{}, unstable: map[string]bool{}}
	leak := func(c matrixCase, phase string, f, r caseResult) {
		run.leaks = append(run.leaks, fmt.Sprintf(
			"%s: state leaked on the shared application (%s): %s %s as %s first returned %d %s, then %d %s",
			c.name, phase, c.method, c.path, c.persona, f.status, f.body, r.status, r.body))
	}

	phase := func(name string, cs []matrixCase, after func(c matrixCase, r caseResult)) {
		t.Run(name, func(t *testing.T) {
			for _, c := range cs {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					got, body := a.do(t, env.m, c)
					if got != c.want {
						t.Errorf("%s %s as %s: got %d, want %d (body: %s)", c.method, c.path, c.persona, got, c.want, body)
					}
					r := caseResult{got, canonicalBody(body)}
					mu.Lock()
					defer mu.Unlock()
					after(c, r)
				})
			}
		})
	}
	record := func(c matrixCase, r caseResult) { run.first[c.name] = r }
	reads := shuffled(g.reads)
	phase("read", reads, record)
	phase("recheck", reads, func(c matrixCase, r caseResult) {
		f := run.first[c.name]
		switch {
		case f.status != r.status:
			leak(c, "recheck", f, r)
		case f.body != r.body:
			run.unstable[c.name] = true
		}
	})
	phase("deny", shuffled(g.denials), record)
	phase("reread", shuffled(g.reads), func(c matrixCase, r caseResult) {
		if f := run.first[c.name]; f.status != r.status || (f.body != r.body && !run.unstable[c.name]) {
			leak(c, "reread", f, r)
		}
	})
	slices.Sort(run.leaks)
	t.Logf("%d of %d reads compared by status only (time or read side effects): %v",
		len(run.unstable), len(g.reads), slices.Sorted(maps.Keys(run.unstable)))
	return run
}

// canonicalBody makes a JSON body comparable across runs: object keys are
// sorted by encoding/json and array elements are sorted, since in-memory
// stores may list rows in map order. Other bodies compare verbatim.
func canonicalBody(body string) string {
	var v any
	if json.Unmarshal([]byte(body), &v) != nil {
		return body
	}
	var canon func(v any) any
	canon = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				x[k] = canon(e)
			}
		case []any:
			keys := make([]string, len(x))
			for i, e := range x {
				x[i] = canon(e)
				b, _ := json.Marshal(x[i])
				keys[i] = string(b)
			}
			slices.Sort(keys)
			out := make([]any, len(keys))
			for i, k := range keys {
				out[i] = json.RawMessage(k)
			}
			return out
		}
		return v
	}
	b, err := json.Marshal(canon(v))
	if err != nil {
		return body
	}
	return string(b)
}

// tokenHash is the credential hash the auth middleware passes to resolvers.
func tokenHash(tok string) [sha256.Size]byte { return sha256.Sum256([]byte(tok)) }
