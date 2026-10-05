package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestSeedDemo_RefusesInProd (05 §13.7, BE-033): `koro seed demo` runs in
// dev and staging only. In prod (or with an unknown or missing APP_ENV) it
// refuses before reading any other setting or opening the database.
func TestSeedDemo_RefusesInProd(t *testing.T) {
	t.Parallel()
	for _, env := range []string{"prod", "production", ""} {
		var stdout, stderr bytes.Buffer
		environ := map[string]string{"APP_ENV": env, "DATABASE_URL": "postgres://nobody@127.0.0.1:1/none?sslmode=disable"}
		if code := run([]string{"seed", "demo"}, environ, &stdout, &stderr); code != 1 {
			t.Fatalf("APP_ENV=%q: exit %d, want 1 (stderr %s)", env, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "dev or staging only") || stdout.Len() != 0 {
			t.Fatalf("APP_ENV=%q: stderr %q stdout %q", env, stderr.String(), stdout.String())
		}
	}
	// dev gets past the refusal (and stops at the missing configuration).
	var stderr bytes.Buffer
	if code := run([]string{"seed", "demo", "-password", "kisa"}, map[string]string{"APP_ENV": "dev"}, &bytes.Buffer{}, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "at least 15 characters") {
		t.Fatalf("short password: exit %d, %s", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"seed", "demo"}, map[string]string{"APP_ENV": "staging"}, &bytes.Buffer{}, &stderr); code != 1 ||
		strings.Contains(stderr.String(), "dev or staging only") {
		t.Fatalf("staging: exit %d, %s", code, stderr.String())
	}
}

// TestSeedReference_RefusesPlaceholdersInProd (06 H-50, T-204): the
// embedded sample reference data has ORNEK-* YÖK codes, which `koro seed
// reference` refuses in prod (before the database is touched); dev loads
// it.
func TestSeedReference_RefusesPlaceholdersInProd(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if code := run([]string{"seed", "reference"}, map[string]string{"APP_ENV": "prod"}, &bytes.Buffer{}, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "placeholder YÖK codes") || !strings.Contains(stderr.String(), "06 H-50") {
		t.Fatalf("prod: exit %d, %s", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"seed", "reference"}, map[string]string{"APP_ENV": "dev"}, &bytes.Buffer{}, &stderr); code != 1 ||
		strings.Contains(stderr.String(), "placeholder") {
		t.Fatalf("dev: exit %d, %s (want the configuration error, not the placeholder refusal)", code, stderr.String())
	}
}
