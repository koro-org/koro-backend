package httpx

import (
	"fmt"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/problem"
)

// The personal query guard (BE-128, KVKK): a URL travels further than the
// request — reverse proxy and access logs (Traefik), browser history,
// Referer headers, error trackers — and none of them is under the access
// and retention rules of the database. So a search criterion that names a
// person never goes into the query string: not directly (an e-mail
// address, a name, a phone number, an IP address, a national or tax ID,
// free text that may be any of these) and not by the person's user ID
// (who did what, when: an activity profile). Such criteria travel in the
// body of a read-only POST …/search operation instead (adminQueryUsers,
// adminQueryEmailSuppressions, …; the terms gate exempts them as reads).
//
// A path parameter is not a criterion: it addresses the one resource the
// caller already holds (/admin/users/{user_id}) and is not checked here.
//
// New installs the guard on every App: each operation is checked when it
// is added to the OpenAPI document (huma's OnAddOperation) and a documented
// query parameter with a personal name panics at registration, unless
// personalQueryAllowed lists the operation and the parameter with its
// reason. `koro api`, `koro openapi` and the tests register every
// operation, so a new one never ships. PersonalQueryViolations checks a
// whole document and PersonalQueryAllowedUnregistered finds stale rows
// (the spec test of cmd/koro).

// personalQueryNames are the query parameter names that carry personal
// data (exact match, lower case).
var personalQueryNames = []string{
	"q", "query", "search", "text", "term",
	"email", "phone", "name", "first_name", "last_name", "full_name",
	"ip", "ip_address", "user_agent", "address", "birth_date",
	"tckn", "national_id", "vkn", "tax_number", "iban",
}

// personalQuerySuffixes are the name endings that do: "<who>_email",
// "<who>_name", "<who>_ip", "actor_user_id" …
var personalQuerySuffixes = []string{"_email", "_phone", "_name", "_ip", "_iban", "user_id"}

// personalQueryAllowed are the operations whose query string may carry a
// parameter with a personal name (operationId → parameter → reason). Each
// reason says why the value is not about a person; a new row needs one of
// the same kind.
var personalQueryAllowed = map[string]map[string]string{
	// Catalog search of public listings: the words of club, event,
	// offering, organization, skill, university and department names and
	// descriptions, which anyone can read in the answer. A person is never
	// a listing.
	"listClubs":        {"q": publicCatalog},
	"listEvents":       {"q": publicCatalog},
	"browseOfferings":  {"q": publicCatalog},
	"searchOrgs":       {"q": publicCatalog},
	"listSkills":       {"q": publicCatalog},
	"listUniversities": {"q": publicCatalog},
	"listDepartments":  {"q": publicCatalog},
	// Organization names (clubs, companies, universities, the platform
	// org): the public profiles searchOrgs and listCompanies show to
	// anyone; the admin list only adds the suspended and closed ones.
	"adminListOrgs": {"q": "organization names; public profiles"},
	// The managers' view of the same catalogs, inactive rows included
	// (BE-128): university and skill names are institutions and skills,
	// never a person.
	"adminListUniversities": {"q": publicCatalog + ", inactive rows included"},
	"adminListSkills":       {"q": publicCatalog + ", inactive rows included"},
}

const publicCatalog = "public catalog text"

// PersonalQueryName reports whether a query parameter name carries
// personal data under the guard's rule.
func PersonalQueryName(name string) bool {
	n := strings.ToLower(name)
	if slices.Contains(personalQueryNames, n) {
		return true
	}
	return slices.ContainsFunc(personalQuerySuffixes, func(s string) bool { return strings.HasSuffix(n, s) })
}

// PersonalQueryAllowed returns a copy of the allowlist (operationId →
// parameter → reason).
func PersonalQueryAllowed() map[string]map[string]string {
	out := make(map[string]map[string]string, len(personalQueryAllowed))
	for id, params := range personalQueryAllowed {
		out[id] = make(map[string]string, len(params))
		for p, why := range params {
			out[id][p] = why
		}
	}
	return out
}

// operationPersonalQuery returns the personal query parameters of op that
// the allowlist does not cover.
func operationPersonalQuery(op *huma.Operation) []string {
	var v []string
	for _, p := range op.Parameters {
		if p == nil || p.In != "query" || !PersonalQueryName(p.Name) {
			continue
		}
		if _, ok := personalQueryAllowed[op.OperationID][p.Name]; ok {
			continue
		}
		v = append(v, fmt.Sprintf("%s (%s %s): query parameter %q", op.OperationID, op.Method, op.Path, p.Name))
	}
	return v
}

// installPersonalQueryGuard adds the guard to oapi's OnAddOperation hooks.
func installPersonalQueryGuard(oapi *huma.OpenAPI) {
	oapi.OnAddOperation = append(oapi.OnAddOperation, func(_ *huma.OpenAPI, op *huma.Operation) {
		if v := operationPersonalQuery(op); len(v) > 0 {
			panic("httpx: personal data in a query string (BE-128; send it in the body of a POST …/search operation): " +
				strings.Join(v, "; "))
		}
	})
}

// PersonalQueryViolations returns every personal query parameter of the
// operations of oapi that the allowlist does not cover, sorted.
func PersonalQueryViolations(oapi *huma.OpenAPI) []string {
	var v []string
	for _, op := range operationsOf(oapi) {
		v = append(v, operationPersonalQuery(op)...)
	}
	slices.Sort(v)
	return v
}

// PersonalQueryAllowedUnregistered returns the allowlist rows
// ("operationId.parameter") whose operation is not in oapi or no longer
// documents that query parameter, sorted.
func PersonalQueryAllowedUnregistered(oapi *huma.OpenAPI) []string {
	seen := map[string]bool{}
	for _, op := range operationsOf(oapi) {
		for _, p := range op.Parameters {
			if p != nil && p.In == "query" {
				seen[op.OperationID+"."+p.Name] = true
			}
		}
	}
	var out []string
	for id, params := range personalQueryAllowed {
		for p := range params {
			if !seen[id+"."+p] {
				out = append(out, id+"."+p)
			}
		}
	}
	slices.Sort(out)
	return out
}

// operationsOf lists the operations of oapi.
func operationsOf(oapi *huma.OpenAPI) []*huma.Operation {
	var out []*huma.Operation
	for _, item := range oapi.Paths {
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Options, item.Head, item.Patch, item.Trace} {
			if op != nil {
				out = append(out, op)
			}
		}
	}
	return out
}

// QueryMoved is the answer of a deprecated list operation to a request
// that still sends a personal criterion in its query string (BE-128): the
// parameter is no longer documented (hidden) and the criterion goes to the
// body of the operation named by to. It is checked with the request's
// syntax, before the operation runs, like a schema rule: a client is told
// to move instead of silently getting an unfiltered list.
func QueryMoved(param, to string) error {
	return problem.Field("query."+param, "personal search criteria are not taken in the URL: send "+param+" in the body of "+to)
}
