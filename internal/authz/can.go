package authz

import (
	"context"
	"fmt"
	"maps"
)

// Can decides whether a may perform action on r (03 §3). It returns nil,
// ErrForbidden, ErrNotFound, ErrMFARequired, ErrFourEyes or
// ErrBreakGlassReason (each wrapping a *Denial). It is pure and fast: every
// HTTP-facing service method calls it before touching its store (05 §5.3),
// workers call it with SystemActor.
func Can(ctx context.Context, a Actor, action Action, r Resource) error {
	return std.can(ctx, a, action, r)
}

// engine is the immutable decision table: known actions, resource types and
// the module policies. std is built once at process start from the
// actions_<module>.go and policy_<module>.go files.
type engine struct {
	http     map[Action]bool
	system   map[Action]bool
	types    map[string][]string // resource type → mandatory Attrs keys
	policies map[string]Policy   // module → policy
}

var std = mustEngine(modulePolicies())

func mustEngine(policies map[string]Policy) *engine {
	e, err := newEngine(HTTPActions(), systemActions, policies)
	if err != nil {
		panic(err)
	}
	return e
}

// newEngine validates and indexes the tables: every module with an HTTP
// action has a policy, and a resource type is declared only once.
func newEngine(httpActions, sysActions []Action, policies map[string]Policy) (*engine, error) {
	e := &engine{
		http:     make(map[Action]bool, len(httpActions)),
		system:   make(map[Action]bool, len(sysActions)),
		types:    maps.Clone(coreTypes),
		policies: maps.Clone(policies),
	}
	for _, a := range httpActions {
		if _, ok := policies[a.Module()]; !ok {
			return nil, fmt.Errorf("authz: module %q has actions but no policy", a.Module())
		}
		e.http[a] = true
	}
	for _, a := range sysActions {
		e.system[a] = true
	}
	for _, m := range sortedKeys(policies) {
		for _, t := range sortedKeys(policies[m].Types) {
			if _, dup := e.types[t]; dup {
				return nil, fmt.Errorf("authz: policy %s redeclares resource type %q", m, t)
			}
			e.types[t] = policies[m].Types[t]
		}
	}
	return e, nil
}

func (e *engine) known(a Action) bool { return e.http[a] || e.system[a] }

// can is the 03 §3.2 decision order (see the package documentation).
func (e *engine) can(_ context.Context, a Actor, act Action, r Resource) error {
	if !e.known(act) {
		return denyInternal(act, "unknown action")
	}
	switch a.Kind {
	case KindSystem:
		return e.canSystem(a, act)
	case KindUser, KindAnonymous:
	default:
		return denyInternal(act, "actor kind "+a.Kind.String())
	}
	if !e.http[act] {
		return deny(ErrForbidden, act, "system-only action (03 §3.7)")
	}
	required, ok := e.types[r.Type]
	if !ok {
		return denyInternal(act, fmt.Sprintf("unknown resource type %q", r.Type))
	}
	for _, k := range required {
		if _, ok := r.Attrs[k]; !ok {
			return denyInternal(act, fmt.Sprintf("resource %s lacks Attrs key %q (03 §3.5)", r.Type, k))
		}
	}
	p := e.policies[act.Module()]
	if p.Grants == nil {
		return denyInternal(act, "policy of module "+act.Module()+" is not written (default deny)")
	}
	grants := p.Grants(a, act, r)
	if a.Kind == KindAnonymous && !hasPublic(grants) {
		return deny(ErrForbidden, act, "anonymous actor on a non-public action")
	}
	if p.Visible == nil || !p.Visible(a, act, r) {
		return deny(ErrNotFound, act, "resource not visible (03 §3.6)")
	}
	if err := checkResourceBans(a, act, r); err != nil {
		return err
	}
	if err := checkFourEyesKeys(act, r); err != nil {
		return err
	}
	grants, err := filterGrants(a, act, r, grants)
	if err != nil {
		return err
	}
	best, bestErr := rankInternal+1, error(nil)
	for _, g := range grants {
		rank, err := checkConditions(a, act, r, g)
		if err == nil {
			return nil
		}
		if rank < best {
			best, bestErr = rank, err
		}
	}
	return bestErr
}

// canSystem is the separate allow-list of 03 §3.7 (not a role path).
func (e *engine) canSystem(a Actor, act Action) error {
	if a.SystemJob == "" {
		return denyInternal(act, "system actor without a job name")
	}
	if !e.system[act] {
		return deny(ErrForbidden, act, "not in the system allow-list (03 §3.7)")
	}
	return nil
}

func hasPublic(gs []Grant) bool {
	for _, g := range gs {
		if g.Role == RolePublic {
			return true
		}
	}
	return false
}
