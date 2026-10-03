package main

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// designLayers is docs/design/01 §5.1 (L1…L13). The module order of
// container.modules() must match it; change the design first, then this
// table (frozen with wire.go after P0).
var designLayers = [][]string{
	{"audit"},
	{"files", "consent", "entitlements"},
	{"notify"},
	{"identity"},
	{"orgs"},
	{"directory"},
	{"students", "parties"},
	{"events", "talent", "outreach"},
	{"feed", "catalog", "discovery"},
	{"messaging", "orders"},
	{"ledger", "billing"},
	{"payments"},
	{"kvkk"},
}

// memStoreUser is implemented by every <module>Wiring in its
// wire_<module>_test.go.
type memStoreUser interface{ useMemStore() }

// newTestContainer returns a container with a fake clock, a discarding
// logger and every module switched to its in-memory stores, not yet built.
func newTestContainer(t *testing.T) *container {
	t.Helper()
	c := newContainer(infra{
		log:     obs.Discard(),
		clock:   clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)),
		version: "test",
	})
	for _, m := range c.modules() {
		u, ok := m.w.(memStoreUser)
		if !ok {
			t.Fatalf("module %s has no useMemStore (wire_%s_test.go)", m.name, m.name)
		}
		u.useMemStore()
	}
	return c
}

func TestWire_LayerOrderMatchesDesign(t *testing.T) {
	t.Parallel()
	type row struct {
		name  string
		layer int
	}
	var want []row
	for i, layer := range designLayers {
		for _, name := range layer {
			want = append(want, row{name, i + 1})
		}
	}
	var got []row
	seen := map[moduleWiring]bool{}
	for _, m := range newContainer(infra{}).modules() {
		got = append(got, row{m.name, m.layer})
		if seen[m.w] {
			t.Fatalf("module %s shares its wiring with another entry", m.name)
		}
		seen[m.w] = true
	}
	if !slices.Equal(got, want) {
		t.Fatalf("container.modules() order differs from 01 §5.1:\n got  %v\n want %v", got, want)
	}
	if len(got) != 22 {
		t.Fatalf("expected 22 feature modules, got %d", len(got))
	}

	// Every *Wiring field of the container is in the table (no module is
	// wired outside the fixed order).
	ct := reflect.TypeFor[container]()
	var fields []string
	for i := range ct.NumField() {
		if f := ct.Field(i); strings.HasSuffix(f.Type.Name(), "Wiring") {
			fields = append(fields, f.Name)
		}
	}
	names := make([]string, len(got))
	for i, r := range got {
		names[i] = r.name
	}
	if !slices.Equal(fields, names) {
		t.Fatalf("container wiring fields %v differ from the module table %v", fields, names)
	}

	// config.Modules has one section per module, in the same order (R7).
	mt := reflect.TypeFor[config.Modules]()
	var sections []string
	for i := range mt.NumField() {
		sections = append(sections, strings.ToLower(mt.Field(i).Name))
	}
	if !slices.Equal(sections, names) {
		t.Fatalf("config.Modules sections %v differ from the module table %v", sections, names)
	}
}

func TestWire_BuildWithMemStores(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatalf("build() error = %v", err)
	}
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: c.actors})
	if err != nil {
		t.Fatal(err)
	}
	c.registerRoutes(app.V1)
}

func TestEnvExample_HasSectionPerModule(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, layer := range designLayers {
		for _, m := range layer {
			if strings.Count(text, "# --- "+m+" ---\n") != 1 {
				t.Errorf(".env.example must contain exactly one %q section header", "# --- "+m+" ---")
			}
		}
	}
	// Sections are separated by blank lines so parallel edits merge cleanly.
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "# --- ") && i > 0 && lines[i-1] != "" {
			t.Errorf(".env.example line %d: section header %q must follow a blank line", i+1, l)
		}
	}
}
