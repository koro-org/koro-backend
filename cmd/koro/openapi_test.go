package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestOpenAPI_Command_WritesValidSpec(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	out := filepath.Join(dir, "nested", "openapi.yaml")
	if err := runOpenAPI([]string{"-o", out}, io.Discard); err != nil {
		t.Fatalf("runOpenAPI() error = %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}
	if spec["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v, want 3.1.0", spec["openapi"])
	}
	info, _ := spec["info"].(map[string]any)
	if info["title"] != "Koro API" || info["version"] != apiVersion {
		t.Fatalf("info = %v", info)
	}
	components, _ := spec["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	problemSchema, _ := schemas["Problem"].(map[string]any)
	props, _ := problemSchema["properties"].(map[string]any)
	for _, p := range []string{"type", "title", "status", "detail", "instance", "code", "errors"} {
		if _, ok := props[p]; !ok {
			t.Errorf("Problem schema lacks %q", p)
		}
	}
	if fe, _ := schemas["FieldError"].(map[string]any); fe != nil {
		if fp, _ := fe["properties"].(map[string]any); fp["value"] != nil {
			t.Error("FieldError must not have a value property")
		}
	}
	sec, _ := components["securitySchemes"].(map[string]any)
	if _, ok := sec["sessionCookie"]; !ok {
		t.Error("sessionCookie security scheme missing")
	}
	if _, ok := sec["bearerAuth"]; !ok {
		t.Error("bearerAuth security scheme missing")
	}

	// problems.json: sorted catalog with types under the default base.
	pb, err := os.ReadFile(filepath.Join(dir, "nested", "problems.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cat []struct {
		Code, Title, Type string
		Status            int
	}
	if err := json.Unmarshal(pb, &cat); err != nil {
		t.Fatalf("problems.json: %v", err)
	}
	codes := map[string]int{}
	for i, e := range cat {
		if i > 0 && cat[i-1].Code >= e.Code {
			t.Fatalf("problems.json not sorted at %q", e.Code)
		}
		if e.Type != "urn:koro:problem:"+e.Code || e.Title == "" {
			t.Fatalf("bad entry %+v", e)
		}
		codes[e.Code] = e.Status
	}
	for code, status := range map[string]int{"validation_failed": 422, "internal_error": 500, "unauthenticated": 401, "csrf_rejected": 403} {
		if codes[code] != status {
			t.Errorf("problems.json %s = %d, want %d", code, codes[code], status)
		}
	}
	// FieldError.code (BE-140): an optional string whose values are
	// problems.json codes (FromError drops any other).
	fe, _ := schemas["FieldError"].(map[string]any)
	fp, _ := fe["properties"].(map[string]any)
	fc, _ := fp["code"].(map[string]any)
	if fc == nil || fc["type"] != "string" {
		t.Fatalf("FieldError.code = %v, want an optional string", fp["code"])
	}
	if req, _ := fe["required"].([]any); slices.Contains(req, any("code")) {
		t.Error("FieldError.code must be optional: entries without their own code have none")
	}
	examples, _ := fc["examples"].([]any)
	for _, ex := range examples {
		if c, _ := ex.(string); codes[c] == 0 {
			t.Errorf("FieldError.code example %v is not in problems.json", ex)
		}
	}
	for code := range codes {
		if strings.HasPrefix(code, "testonly.") {
			t.Errorf("test-only code %q leaked into the catalog", code)
		}
	}

	// Deterministic: a second run yields identical bytes (make openapi-check).
	spec2, problems2, err := renderOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, spec2) || !bytes.Equal(pb, problems2) {
		t.Fatal("koro openapi output is not deterministic")
	}
}

func TestOpenAPI_Command_RejectsExtraArgs(t *testing.T) {
	t.Parallel()
	if got := run([]string{"openapi", "-o", filepath.Join(t.TempDir(), "x.yaml"), "extra"}, nil, io.Discard, io.Discard); got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
}
