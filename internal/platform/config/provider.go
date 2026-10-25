package config

import (
	"fmt"
	"slices"
	"strings"
)

// Adapter selection (BE-113). Every external system is a port with one or
// more adapters; one variable per port names the adapter
// (<SECTION>_PROVIDER: NOTIFY_EMAIL_PROVIDER, STORAGE_PROVIDER,
// NOTIFY_PUSH_PROVIDER; FILES_SCANNER predates the rule and keeps its name)
// and cmd/koro builds it in one place (the selection map of the port's
// wire_<module>.go). A new adapter is a new file next to the port, its name
// here and one line in that map; it must pass the port's contract suite.
//
// ProviderMem and ProviderNoop are the development and test adapters. They
// never run in staging or prod: there, choosing one stops the process at
// start (05 §12.2: no silent in-memory substitute).
const (
	ProviderMem  = "mem"
	ProviderNoop = "noop"
)

// checkProvider normalises *v (trimmed, lower case) and checks that it is
// one of allowed and, in staging and prod, not a development adapter.
func checkProvider(c Common, key string, v *string, allowed []string) error {
	*v = strings.ToLower(strings.TrimSpace(*v))
	if !slices.Contains(allowed, *v) {
		return fmt.Errorf("%s must be %s (got %q)", key, orList(allowed), *v)
	}
	if c.ProdLike() && isDevAdapter(*v) {
		return fmt.Errorf("%s=%s is a development adapter, which is not allowed in %s", key, *v, c.Env)
	}
	return nil
}

func isDevAdapter(name string) bool { return name == ProviderMem || name == ProviderNoop }

// orList renders "a", "a or b", "a, b or c".
func orList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
