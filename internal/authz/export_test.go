package authz

import (
	"context"
	"maps"
)

// CanWith returns a Can that uses the real action and type tables but the
// given policies; modules not in policies keep their real policy. It lets
// tests prove the core rules (absolute bans, conditions) against a policy
// that grants everything.
func CanWith(policies map[string]Policy) func(ctx context.Context, a Actor, act Action, r Resource) error {
	ps := modulePolicies()
	maps.Copy(ps, policies)
	return mustEngine(ps).can
}

// NewEngineErr reports whether the policy table is rejected by newEngine.
func NewEngineErr(policies map[string]Policy) error {
	ps := modulePolicies()
	maps.Copy(ps, policies)
	_, err := newEngine(HTTPActions(), systemActions, ps)
	return err
}

// PolicyWritten reports whether a module's policy is no longer the stub.
func PolicyWritten(module string) bool { return modulePolicies()[module].Grants != nil }

// CoreTypes exposes the 03 §3.5 table.
func CoreTypes() map[string][]string { return maps.Clone(coreTypes) }

// ResourceTypes exposes every resource type of the decision table (03 §3.5
// and the module-declared ones) with its mandatory Attrs keys.
func ResourceTypes() map[string][]string { return maps.Clone(std.types) }

// PolicyGrants returns the role paths act's module policy gives a on r,
// before the core's checks (holds, bans, conditions).
func PolicyGrants(a Actor, act Action, r Resource) []Grant {
	p := std.policies[act.Module()]
	if p.Grants == nil {
		return nil
	}
	return p.Grants(a, act, r)
}

// StaffPath reports whether g is a platform role's path.
func StaffPath(g Grant) bool { return staffPaths[g.Role] }
