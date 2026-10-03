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
