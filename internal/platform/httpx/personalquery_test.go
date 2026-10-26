package httpx_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

type queryEmail struct {
	Email string `query:"email"`
}

type queryActor struct {
	ActorUserID string `query:"actor_user_id"`
}

type queryFreeText struct {
	Q string `query:"q"`
}

type querySenderName struct {
	SenderName string `query:"sender_name"`
}

type queryHarmless struct {
	Status string `query:"status"`
	OrgID  string `query:"org_id"`
	Limit  int    `query:"limit"`
}

// queryHiddenQ is a deprecated list that rejects q (httpx.QueryMoved).
type queryHiddenQ struct {
	Q string `query:"q" hidden:"true"`
}

type pathUser struct {
	UserID string `path:"user_id"`
}

// TestPersonalQueryGuard_RejectsPersonalCriteria (BE-128): a documented
// query parameter that names a person — an e-mail address, free text, a
// user ID, a "<who>_name" — stops the registration, whatever the method,
// unless the allowlist covers that operation and parameter. A hidden
// parameter (the deprecated twin that only rejects it) and a path
// parameter pass, and so do non-personal filters.
func TestPersonalQueryGuard_RejectsPersonalCriteria(t *testing.T) {
	t.Parallel()
	for name, reg := range map[string]func(huma.API) string{
		"email":         func(api huma.API) string { return register[queryEmail, guardEmpty](api, guardOp("listMail", "notify")) },
		"actor_user_id": func(api huma.API) string { return register[queryActor, guardEmpty](api, guardOp("listActs", "audit")) },
		"q": func(api huma.API) string {
			return register[queryFreeText, guardEmpty](api, guardOp("listPeople", "identity"))
		},
		"sender_name": func(api huma.API) string {
			return register[querySenderName, guardEmpty](api, guardOp("listMoney", "payments"))
		},
		"listClubs with email": func(api huma.API) string {
			return register[queryEmail, guardEmpty](api, guardOp("listClubs", "directory")) // allowed q, not email
		},
	} {
		msg := reg(guardApp(t))
		if msg == "" || !strings.Contains(msg, "personal data in a query string") {
			t.Errorf("%s: registration did not panic with the guard's message (%q)", name, msg)
		}
	}

	api := guardApp(t)
	for name, msg := range map[string]string{
		"allowlisted q": register[queryFreeText, guardEmpty](api, guardOp("listClubs", "directory")),
		"harmless":      register[queryHarmless, guardEmpty](api, guardOp("listThings", "orgs")),
		"hidden q":      register[queryHiddenQ, guardEmpty](api, guardOp("listOldPeople", "identity")),
		"path user_id":  register[pathUser, guardEmpty](api, huma.Operation{OperationID: "getPerson", Method: http.MethodGet, Path: "/people/{user_id}", Tags: []string{"identity"}}),
	} {
		if msg != "" {
			t.Errorf("%s: rejected: %s", name, msg)
		}
	}
	if v := httpx.PersonalQueryViolations(api.OpenAPI()); len(v) != 0 {
		t.Fatalf("violations = %q, want none", v)
	}
	// A document assembled without the hook is still caught.
	oapi := api.OpenAPI()
	oapi.Paths["/sneaky"] = &huma.PathItem{Get: &huma.Operation{OperationID: "sneaky", Method: http.MethodGet, Path: "/sneaky",
		Parameters: []*huma.Param{{Name: "Phone", In: "query", Schema: &huma.Schema{Type: "string"}}}}}
	if v := httpx.PersonalQueryViolations(oapi); len(v) != 1 || !strings.Contains(v[0], `"Phone"`) {
		t.Fatalf("violations = %q, want the sneaky parameter", v)
	}
	// Every allowlist row but listClubs.q is stale in this document.
	stale := httpx.PersonalQueryAllowedUnregistered(oapi)
	if want := countAllowed() - 1; len(stale) != want || strings.Contains(strings.Join(stale, ","), "listClubs.q") {
		t.Fatalf("stale rows = %v, want %d without listClubs.q", stale, want)
	}
}

func countAllowed() int {
	n := 0
	for _, params := range httpx.PersonalQueryAllowed() {
		n += len(params)
	}
	return n
}

// TestPersonalQueryName pins the rule's names and suffixes, and that the
// allowlist rows give their reason.
func TestPersonalQueryName(t *testing.T) {
	t.Parallel()
	for _, n := range []string{"q", "Q", "email", "contact_email", "name", "sender_name", "phone", "ip", "client_ip",
		"user_agent", "actor_user_id", "user_id", "tckn", "vkn", "iban", "sender_iban", "birth_date"} {
		if !httpx.PersonalQueryName(n) {
			t.Errorf("PersonalQueryName(%q) = false", n)
		}
	}
	for _, n := range []string{"status", "kind", "org_id", "university_id", "resource_id", "order_no", "limit", "cursor",
		"occurred_from", "sort", "category", "city"} {
		if httpx.PersonalQueryName(n) {
			t.Errorf("PersonalQueryName(%q) = true", n)
		}
	}
	for id, params := range httpx.PersonalQueryAllowed() {
		for p, why := range params {
			if strings.TrimSpace(why) == "" || !httpx.PersonalQueryName(p) {
				t.Errorf("allowlist row %s.%s: reason %q (the parameter must be one the rule catches)", id, p, why)
			}
		}
	}
}

// TestQueryMoved: the deprecated twin's answer names the parameter and
// the operation that takes it.
func TestQueryMoved(t *testing.T) {
	t.Parallel()
	err := httpx.QueryMoved("q", "adminQueryUsers")
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	p, _ := errors.AsType[*problem.Error](err)
	if f := p.Fields(); len(f) != 1 || f[0].Location != "query.q" || !strings.Contains(f[0].Message, "adminQueryUsers") {
		t.Fatalf("fields = %+v", f)
	}
}
