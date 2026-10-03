package main

// The "testonly" module of the matrix self-tests: a handful of synthetic
// operations, registered only here, whose handlers follow the 404/403/401
// rules of 03 §7.2 so the harness can be proven end to end (BE-004). Its
// matrix lives in testdata/authz_selftest/ and is merged with the real
// testdata/authz/_personas.yaml and _platform.yaml.

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
)

// testOnlyActions are the actions of the synthetic module; .archive is
// deliberately unbound and listed under pending_actions.
var testOnlyActions = []string{
	"testonly.thing.list", "testonly.thing.read", "testonly.thing.create", "testonly.thing.deny",
	"testonly.thing.archive", "testonly.admin_thing.create", "testonly.admin_thing.approve", "testonly.flagged.read",
}

type testOnlyThing struct {
	org, createdBy uuid.UUID
	approved       bool
}

type testOnlyState struct {
	mu     sync.Mutex
	things map[uuid.UUID]*testOnlyThing
}

func (s *testOnlyState) get(id uuid.UUID) (testOnlyThing, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	th, ok := s.things[id]
	if !ok {
		return testOnlyThing{}, false
	}
	return *th, true
}

// selfTestEnv returns the harness options of the synthetic module.
func selfTestEnv() *matrixEnv { return newSelfTestEnv(false) }

// newSelfTestEnv: leakyDenials makes testOnlyCreateThing create the thing
// before it denies the request (a write-then-deny bug the shared
// application must expose, BE-060).
func newSelfTestEnv(leakyDenials bool) *matrixEnv {
	states := sync.Map{} // *matrixApp → *testOnlyState
	state := func(a *matrixApp) *testOnlyState {
		s, _ := states.LoadOrStore(a, &testOnlyState{things: map[uuid.UUID]*testOnlyThing{}})
		return s.(*testOnlyState)
	}
	return &matrixEnv{opts: matrixOptions{
		modules: []string{"testonly"},
		catalog: actionCatalog{
			known:    func(a string) bool { return slices.Contains(testOnlyActions, a) },
			coverage: testOnlyActions,
		},
		configKeys: map[string]bool{"testonly_flag": true},
		fixtures: map[string]func(a *matrixApp) error{"testonly": func(a *matrixApp) error {
			s := state(a)
			for _, f := range a.fx.Of("testonly") {
				org, err := a.fx.Ref(f, "org")
				if err != nil {
					return err
				}
				by, err := a.fx.Ref(f, "created_by")
				if err != nil {
					return err
				}
				id, _ := a.fx.ID(f.Name)
				s.things[id] = &testOnlyThing{org: org, createdBy: by}
			}
			return nil
		}},
		hooks: map[string]func(a *matrixApp, name string, args map[string]any) error{
			"testonly": func(a *matrixApp, name string, args map[string]any) error {
				if name != "delete_thing" {
					return fmt.Errorf("unknown hook %s", name)
				}
				ref, _ := args["thing"].(string)
				id, err := a.fx.ID(ref)
				if err != nil {
					return err
				}
				s := state(a)
				s.mu.Lock()
				delete(s.things, id)
				s.mu.Unlock()
				return nil
			},
		},
		routes: func(a *matrixApp) { registerTestOnly(a, state(a), leakyDenials) },
	}}
}

type thingIDBody struct {
	Body struct {
		ID string `json:"id"`
	}
}

type thingCountBody struct {
	Body struct {
		Count int `json:"count"`
	}
}

func testOnlyOp(id, method, path string, m op.Meta) huma.Operation {
	o := huma.Operation{OperationID: id, Method: method, Path: path, Tags: []string{"testonly"}}
	if strings.HasSuffix(string(m.Action), ".create") {
		o.DefaultStatus = http.StatusCreated
	}
	return op.Koro(o, m)
}

// registerTestOnly registers the synthetic operations and a raw WebSocket
// route (non_api_routes).
func registerTestOnly(a *matrixApp, s *testOnlyState, leakyDenials bool) {
	api := a.app.V1
	actor := func(ctx context.Context) authz.Actor { x, _ := authz.ActorFrom(ctx); return x }

	huma.Register(api, testOnlyOp("testOnlyListThings", http.MethodGet, "/testonly/things",
		op.Meta{Action: "testonly.thing.list", Surface: op.SurfacePublic}),
		func(ctx context.Context, _ *struct{}) (*thingCountBody, error) {
			o := &thingCountBody{}
			s.mu.Lock()
			o.Body.Count = len(s.things)
			s.mu.Unlock()
			return o, nil
		})

	// Org surface (03 §3.6, §7.6): non-members 404, members without the role
	// 403, club_admin allowed, company_admin allowed with MFA.
	orgThing := func(a authz.Actor, orgRaw, thingRaw string) (testOnlyThing, error) {
		org, err1 := uuid.Parse(orgRaw)
		id, err2 := uuid.Parse(thingRaw)
		if err1 != nil || err2 != nil || !authz.OrgSurfaceVisible(a, &org) {
			return testOnlyThing{}, authz.ErrNotFound
		}
		th, ok := s.get(id)
		if !ok || th.org != org {
			return testOnlyThing{}, authz.ErrNotFound
		}
		switch {
		case a.HasOrgRole(org, authz.MemberClubAdmin):
			return th, nil
		case a.HasOrgRole(org, authz.MemberCompanyAdmin, authz.MemberCompanyOwner) && !a.MFAVerified:
			return th, authz.ErrMFARequired
		case a.HasOrgRole(org, authz.MemberCompanyAdmin, authz.MemberCompanyOwner):
			return th, nil
		}
		return th, authz.ErrForbidden
	}
	type getIn struct {
		OrgID   string `path:"org_id"`
		ThingID string `path:"thing_id"`
	}
	huma.Register(api, testOnlyOp("testOnlyGetThing", http.MethodGet, "/testonly/orgs/{org_id}/things/{thing_id}",
		op.Meta{Action: "testonly.thing.read", Surface: op.SurfaceOrg}),
		func(ctx context.Context, in *getIn) (*thingIDBody, error) {
			if _, err := orgThing(actor(ctx), in.OrgID, in.ThingID); err != nil {
				return nil, err
			}
			o := &thingIDBody{}
			o.Body.ID = in.ThingID
			return o, nil
		})

	type createIn struct {
		OrgID          string `path:"org_id"`
		IdempotencyKey string `header:"Idempotency-Key" required:"true"`
		Body           struct {
			Name string `json:"name" minLength:"1"`
		}
	}
	huma.Register(api, testOnlyOp("testOnlyCreateThing", http.MethodPost, "/testonly/orgs/{org_id}/things",
		op.Meta{Action: "testonly.thing.create", Surface: op.SurfaceOrg, Idempotency: op.IdempotencyRequired}),
		func(ctx context.Context, in *createIn) (*thingIDBody, error) {
			a := actor(ctx)
			org, err := uuid.Parse(in.OrgID)
			if leakyDenials && err == nil {
				createThing(s, org, a.UserID)
			}
			if err != nil || !authz.OrgSurfaceVisible(a, &org) {
				return nil, authz.ErrNotFound
			}
			if !a.HasOrgRole(org, authz.MemberClubAdmin) {
				return nil, authz.ErrForbidden
			}
			return createThing(s, org, a.UserID), nil
		})

	// Admin surface (03 §7.2-3): without a platform role 403 before reading.
	type adminIn struct {
		Body struct {
			Name string `json:"name" minLength:"1"`
		}
	}
	huma.Register(api, testOnlyOp("testOnlyAdminCreateThing", http.MethodPost, "/testonly/admin/things",
		op.Meta{Action: "testonly.admin_thing.create", Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}),
		func(ctx context.Context, _ *adminIn) (*thingIDBody, error) {
			a := actor(ctx)
			if !a.HasPlatformRole(authz.PlatformFinanceAdmin) || !a.MFAVerified {
				return nil, authz.ErrForbidden
			}
			return createThing(s, authztest.ID("platform_org"), a.UserID), nil
		})
	type approveIn struct {
		ThingID string `path:"thing_id"`
	}
	huma.Register(api, testOnlyOp("testOnlyApproveThing", http.MethodPost, "/testonly/admin/things/{thing_id}/approve",
		op.Meta{Action: "testonly.admin_thing.approve", Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}),
		func(ctx context.Context, in *approveIn) (*thingIDBody, error) {
			a := actor(ctx)
			if !a.HasAnyPlatformRole() {
				return nil, authz.ErrForbidden
			}
			id, err := uuid.Parse(in.ThingID)
			th, ok := s.get(id)
			if err != nil || !ok {
				return nil, authz.ErrNotFound
			}
			switch {
			case !a.HasPlatformRole(authz.PlatformFinanceAdmin):
				return nil, authz.ErrForbidden
			case th.createdBy == a.UserID:
				return nil, authz.ErrFourEyes
			}
			s.mu.Lock()
			s.things[id].approved = true
			s.mu.Unlock()
			o := &thingIDBody{}
			o.Body.ID = in.ThingID
			return o, nil
		})

	// Config flag (mirrors talent_auditor_api_enabled, 03 §7.2-2).
	huma.Register(api, testOnlyOp("testOnlyGetFlagged", http.MethodGet, "/testonly/admin/flagged",
		op.Meta{Action: "testonly.flagged.read", Surface: op.SurfaceAdmin}),
		func(ctx context.Context, _ *struct{}) (*thingCountBody, error) {
			if on, _ := a.cfg["testonly_flag"].(bool); !on {
				return nil, authz.ErrNotFound
			}
			if !actor(ctx).HasPlatformRole(authz.PlatformTalentAuditor) {
				return nil, authz.ErrForbidden
			}
			return &thingCountBody{}, nil
		})

	// The real authz.Can on a system-only action: denied for every user.
	huma.Register(api, testOnlyOp("testOnlyDefaultDeny", http.MethodGet, "/testonly/default-deny",
		op.Meta{Action: "testonly.thing.deny", Surface: op.SurfaceSelf}),
		func(ctx context.Context, _ *struct{}) (*thingCountBody, error) {
			if err := authz.Can(ctx, actor(ctx), authz.KvkkRetentionSweep, authz.Resource{Type: authz.TypeAdminSurface}); err != nil {
				return nil, err
			}
			return &thingCountBody{}, nil
		})

	// Raw gin WebSocket route: 401 without a credential, 101 for club_a's
	// club_admin, 403 otherwise.
	a.app.Engine.GET(httpx.APIPrefix+"/testonly/live", func(c *gin.Context) {
		tok, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok {
			c.Status(http.StatusUnauthorized)
			return
		}
		ctx, err := a.c.actors.Resolve(c.Request.Context(), httpx.Credential{Transport: httpx.TransportBearer, TokenHash: tokenHash(tok)})
		if err != nil {
			c.Status(http.StatusUnauthorized)
			return
		}
		if !actor(ctx).HasOrgRole(authztest.ID("club_a"), authz.MemberClubAdmin) {
			c.Status(http.StatusForbidden)
			return
		}
		conn, rw, err := c.Writer.Hijack()
		if err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
	})
}

func createThing(s *testOnlyState, org, by uuid.UUID) *thingIDBody {
	id := uuid.NewV7()
	s.mu.Lock()
	s.things[id] = &testOnlyThing{org: org, createdBy: by}
	s.mu.Unlock()
	o := &thingIDBody{}
	o.Body.ID = id.String()
	return o
}

// selfTestFS merges the real persona and platform files with the synthetic
// module files (testdata/authz_selftest).
func selfTestFS(t *testing.T) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	for _, f := range []string{"_personas.yaml", "_platform.yaml"} {
		b, err := os.ReadFile(filepath.Join("testdata", "authz", f))
		if err != nil {
			t.Fatal(err)
		}
		fsys[f] = &fstest.MapFile{Data: b}
	}
	root := filepath.Join("testdata", "authz_selftest")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fsys[filepath.ToSlash(rel)] = &fstest.MapFile{Data: b}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

// edit returns a copy of fsys with one file rewritten by a replacement
// (old must occur) or removed (newText == "" and old == "").
func edit(t *testing.T, fsys fstest.MapFS, file, old, newText string) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	for k, v := range fsys {
		out[k] = &fstest.MapFile{Data: append([]byte(nil), v.Data...)}
	}
	if old == "" && newText == "" {
		delete(out, file)
		return out
	}
	f, ok := out[file]
	switch {
	case !ok && old == "":
		out[file] = &fstest.MapFile{Data: []byte(newText)}
	case !ok:
		t.Fatalf("edit: no file %s", file)
	case !strings.Contains(string(f.Data), old):
		t.Fatalf("edit: %s does not contain %q", file, old)
	default:
		f.Data = []byte(strings.Replace(string(f.Data), old, newText, 1))
	}
	return out
}

// selfTestProblems loads fsys with the synthetic module and returns the
// load and static-check problems.
func selfTestProblems(t *testing.T, fsys fs.FS, extra func(a *matrixApp)) []string {
	t.Helper()
	env := selfTestEnv()
	if extra != nil {
		routes := env.opts.routes
		env.opts.routes = func(a *matrixApp) { routes(a); extra(a) }
	}
	m, probs := loadMatrix(fsys, env.opts.modules)
	env.m = m
	spec, routes := env.specAndRoutes(t)
	more, _ := checkMatrix(env.checkInput(spec, routes))
	return append(probs, more...)
}

func wantProblem(t *testing.T, probs []string, substr string) {
	t.Helper()
	for _, p := range probs {
		if strings.Contains(p, substr) {
			return
		}
	}
	t.Fatalf("no problem contains %q; got:\n%s", substr, strings.Join(probs, "\n"))
}
