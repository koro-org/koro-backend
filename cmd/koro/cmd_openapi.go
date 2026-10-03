package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// runOpenAPI runs `koro openapi -o openapi/openapi.yaml`: it registers every
// module's operations with nil services (no DB, no Redis) and writes the
// OpenAPI 3.1 document plus problems.json (the error code catalog) next to
// it. The output is deterministic; CI diffs it (make openapi-check).
func runOpenAPI(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("openapi", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "openapi/openapi.yaml", "OpenAPI YAML output file; problems.json is written to the same directory")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected arguments %q", errUsage, fs.Args())
	}

	spec, problems, err := renderOpenAPI()
	if err != nil {
		return err
	}
	dir := filepath.Dir(*out)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.WriteFile(*out, spec, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	pPath := filepath.Join(dir, "problems.json")
	if err := os.WriteFile(pPath, problems, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", pPath, err)
	}
	return nil
}

// renderOpenAPI builds the HTTP application exactly as `koro api` does,
// minus infrastructure, and returns the YAML spec and the JSON catalog.
func renderOpenAPI() (spec, problems []byte, err error) {
	log := obs.Discard()
	app, err := httpx.New(httpx.Deps{Log: log, Version: apiVersion})
	if err != nil {
		return nil, nil, err
	}
	c := newContainer(infra{log: log, clock: clock.Real(), version: version})
	c.registerRoutes(app.V1)

	spec, err = app.API.OpenAPI().YAML()
	if err != nil {
		return nil, nil, fmt.Errorf("render openapi: %w", err)
	}
	problems, err = json.MarshalIndent(problem.Catalog(problem.DefaultTypeBase), "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("render problems: %w", err)
	}
	return spec, append(problems, '\n'), nil
}
