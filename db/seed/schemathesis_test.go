package seed_test

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"koro/db/seed"
)

// TestDemoPathParameters_PointAtReadableRows (BE-171): every path
// parameter given to Schemathesis is a planned row of the right table that
// the demo company owner (or anyone, for the public ones) may read, so the
// fuzzer reaches real 200 responses.
func TestDemoPathParameters_PointAtReadableRows(t *testing.T) {
	t.Parallel()
	p := build(t, reference(), now)
	params := p.Demo.PathParameters()
	find := func(table, col, val string) map[string]any {
		for _, r := range rows(p, table) {
			if fmt.Sprint(r[col]) == val {
				return r
			}
		}
		return nil
	}
	want := map[string]struct {
		table, col string
		check      func(map[string]any) bool
	}{
		"org_id":   {"organizations", "id", func(r map[string]any) bool { return r["kind"] == "company" && r["status"] == "active" }},
		"slug":     {"organizations", "slug", func(r map[string]any) bool { return fmt.Sprint(r["id"]) == params["org_id"] }},
		"club_id":  {"clubs", "org_id", nil},
		"event_id": {"events", "id", func(r map[string]any) bool { return r["status"] == "published" && r["visibility"] == "public" }},
		"post_id": {"posts", "id", func(r map[string]any) bool {
			return fmt.Sprint(r["org_id"]) == params["org_id"] && r["audience_kind"] == "public"
		}},
		"notice_version_id": {"notice_versions", "id", nil},
		"segment_id":        {"segments", "id", func(r map[string]any) bool { return fmt.Sprint(r["org_id"]) == params["org_id"] }},
		"entitlement_id":    {"entitlements", "id", func(r map[string]any) bool { return fmt.Sprint(r["org_id"]) == params["org_id"] }},
		"party_id":          {"legal_parties", "id", func(r map[string]any) bool { return fmt.Sprint(r["org_id"]) == params["org_id"] }},
		"offering_id":       {"offerings", "id", nil},
		"order_id":          {"orders", "id", func(r map[string]any) bool { return fmt.Sprint(r["org_id"]) == params["org_id"] }},
	}
	for name, w := range want {
		r := find(w.table, w.col, params[name])
		if r == nil {
			t.Errorf("%s=%q: no %s row with %s", name, params[name], w.table, w.col)
			continue
		}
		if w.check != nil && !w.check(r) {
			t.Errorf("%s=%q: the %s row is not readable by the company owner: %v", name, params[name], w.table, r)
		}
	}
	u := p.Demo.MainUniversity
	if params["university_id"] != u.ID.String() || params["department_id"] != u.Departments[0].ID.String() {
		t.Errorf("university/department: %q %q", params["university_id"], params["department_id"])
	}
	if len(params) != len(want)+2 {
		t.Errorf("%d parameters, want %d (a new one needs a check here)", len(params), len(want)+2)
	}
}

// TestDemoPathParameters_AreSpecPathParameters (BE-171): every name is a
// path parameter of the served OpenAPI document, so a renamed parameter
// does not silently fall back to random values.
func TestDemoPathParameters_AreSpecPathParameters(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../openapi/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []struct{ Name, In string } `yaml:"parameters"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	inPath := map[string]bool{}
	for _, item := range doc.Paths {
		for _, op := range item {
			for _, prm := range op.Parameters {
				if prm.In == "path" {
					inPath[prm.Name] = true
				}
			}
		}
	}
	for name := range build(t, reference(), now).Demo.PathParameters() {
		if !inPath[name] {
			t.Errorf("%q is not a path parameter of openapi/openapi.yaml", name)
		}
	}
}

// TestWriteSchemathesisConfig (BE-171): a [parameters] table of sorted,
// location-qualified "path.<name>" keys with quoted values.
func TestWriteSchemathesisConfig(t *testing.T) {
	t.Parallel()
	d := build(t, reference(), now).Demo
	var buf bytes.Buffer
	if err := seed.WriteSchemathesisConfig(&buf, d); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if !strings.HasPrefix(lines[0], "# ") || lines[1] != "[parameters]" {
		t.Fatalf("header: %q", lines[:2])
	}
	entry := regexp.MustCompile(`^"path\.([a-z_]+)" = ("[^"\\]+")$`)
	params := d.PathParameters()
	var names []string
	for _, l := range lines[2:] {
		m := entry.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("line %q is not a quoted path parameter", l)
		}
		v, err := strconv.Unquote(m[2])
		if err != nil || v != params[m[1]] || v == "" {
			t.Fatalf("%s = %q (%v), want %q", m[1], v, err, params[m[1]])
		}
		names = append(names, m[1])
	}
	if len(names) != len(params) || !slices.IsSorted(names) {
		t.Fatalf("names %v", names)
	}
}
