package orgs_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"uuid"

	"koro/internal/orgs"
)

// TestHTTP_ListCompanies_QAndCity (BE-184): q and city narrow the public
// company list and a suspended company stays out of it with either (it is
// searchOrgs?kind=company that shows it); a bad q is 422, a cursor belongs
// to its filter.
func TestHTTP_ListCompanies_QAndCity(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	add := func(name, city string, st orgs.Status) {
		t.Helper()
		v, _ := e.company(t, name)
		o, err := e.store.GetOrg(t.Context(), v.Org.ID)
		if err != nil {
			t.Fatal(err)
		}
		o.Status, o.City = st, new(city)
		if _, err := e.store.UpdateOrg(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
	add("Deniz Yazılım", "İzmir", orgs.StatusActive)
	add("Deniz Lojistik", "Ankara", orgs.StatusActive)
	add("Dağ Yazılım", "İzmir", orgs.StatusActive)
	add("Deniz Askıda", "İzmir", orgs.StatusSuspended)
	add("Deniz Beklemede", "İzmir", orgs.StatusPendingReview)
	e.club(t, "Deniz Kulübü", "open", false)
	app := newHTTP(t, e)

	get := func(path string, query url.Values) (names []string, next string) {
		t.Helper()
		rec := call(t, app, "anonymous", http.MethodGet, path+"?"+query.Encode(), "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %v = %d %s", path, query, rec.Code, rec.Body)
		}
		var p orgs.OrgCardList
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		for _, it := range p.Items {
			names = append(names, it.Name)
		}
		return names, p.NextCursor
	}
	for _, tc := range []struct {
		q, city string
		want    []string
	}{
		{"deniz", "", []string{"Deniz Lojistik", "Deniz Yazılım"}},
		{"", "izmir", []string{"Dağ Yazılım", "Deniz Yazılım"}},
		{"yazılım", "İZMİR", []string{"Dağ Yazılım", "Deniz Yazılım"}},
		{"deniz", "ankara", []string{"Deniz Lojistik"}},
		{"askıda", "", nil},
		{"", "Bursa", nil},
	} {
		got, _ := get("/api/v1/companies", url.Values{"q": {tc.q}, "city": {tc.city}, "sort": {"name"}})
		if !slices.Equal(got, tc.want) {
			t.Errorf("q=%q city=%q: %q, want %q", tc.q, tc.city, got, tc.want)
		}
	}
	// searchOrgs stays as it was: the suspended company is listed there.
	if got, _ := get("/api/v1/orgs", url.Values{"q": {"askıda"}, "kind": {"company"}}); !slices.Equal(got, []string{"Deniz Askıda"}) {
		t.Fatalf("searchOrgs = %q", got)
	}

	// The filter pages through and the cursor is bound to it.
	first, cursor := get("/api/v1/companies", url.Values{"q": {"deniz"}, "sort": {"name"}, "limit": {"1"}})
	if !slices.Equal(first, []string{"Deniz Lojistik"}) || cursor == "" {
		t.Fatalf("page 1 = %q, cursor %q", first, cursor)
	}
	second, last := get("/api/v1/companies", url.Values{"q": {"deniz"}, "sort": {"name"}, "limit": {"1"}, "cursor": {cursor}})
	if !slices.Equal(second, []string{"Deniz Yazılım"}) || last != "" {
		t.Fatalf("page 2 = %q, cursor %q", second, last)
	}
	for name, q := range map[string]url.Values{
		"cursor of another q":       {"q": {"yazılım"}, "sort": {"name"}, "cursor": {cursor}},
		"cursor of another city":    {"q": {"deniz"}, "city": {"İzmir"}, "sort": {"name"}, "cursor": {cursor}},
		"q too short":               {"q": {"d"}},
		"control character in q":    {"q": {"de\u202eniz"}},
		"control character in city": {"city": {"İz\u202emir"}},
	} {
		if rec := call(t, app, "anonymous", http.MethodGet, "/api/v1/companies?"+q.Encode(), "", nil); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d %s, want 422", name, rec.Code, rec.Body)
		}
	}
}

// TestActiveRoleHolders (BE-184): the holders of a role by org, read once;
// an org without one has no key.
func TestActiveRoleHolders(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.club(t, "Rol A", "open", false)
	b := e.club(t, "Rol B", "open", false)
	c := e.club(t, "Rol C", "open", false)
	admin := e.member(t, a, "club_admin")
	e.member(t, a, "member")
	e.member(t, c, "club_admin")
	got, err := e.svc.ActiveRoleHolders(t.Context(), []uuid.UUID{a, b}, "club_admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !slices.Equal(got[a], []uuid.UUID{admin.UserID}) {
		t.Fatalf("holders = %v", got)
	}
}
