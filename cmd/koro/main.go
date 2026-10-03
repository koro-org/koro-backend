// Command koro is the single Koro binary (06 H-8). Subcommands:
//
//	koro api                 HTTP API server
//	koro worker              background worker (BE-006)
//	koro migrate up|down N|version|force V|freeze V   database migrations (owner role)
//	koro seed ...            synthetic data, dev/staging only (BE-020, BE-033)
//	koro openapi -o FILE     write the OpenAPI 3.1 spec and problems.json (no DB/Redis)
//	koro healthcheck         Docker HEALTHCHECK: GET /healthz on the local API
//
// cmd/koro is the composition root: wire.go is the only place that imports
// every module. It contains no business logic, SQL or HTTP rules.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // distroless has no tzdata; Europe/Istanbul must always load (05 §9.2)

	"koro/internal/platform/config"
)

// version is set at build time: -ldflags "-X main.version=<git sha>".
var version = "dev"

// errUsage marks command-line mistakes (exit code 2).
var errUsage = errors.New("usage")

// errNotImplemented marks subcommands whose ticket has not landed yet.
var errNotImplemented = errors.New("not implemented yet")

const usage = `usage: koro <command> [arguments]

commands:
  api            run the HTTP API server
  worker         run the background worker (not implemented yet: BE-006)
  migrate        run database migrations (up | down N | version | force V | freeze V)
  seed           load synthetic data, dev/staging only (not implemented yet: BE-020/BE-033)
  openapi        write openapi/openapi.yaml and openapi/problems.json (-o FILE)
  healthcheck    probe the local API's /healthz (Docker HEALTHCHECK)
`

func main() {
	os.Exit(run(os.Args[1:], config.Environ(), os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the process exit code. environ is
// a copy of the process environment (tests pass their own map).
func run(args []string, environ map[string]string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "api":
		err = runAPI(ctx, stop, environ, stdout)
	case "worker":
		err = runWorker(ctx, rest)
	case "migrate":
		err = runMigrate(ctx, rest, environ, stdout)
	case "seed":
		err = runSeed(ctx, rest)
	case "openapi":
		err = runOpenAPI(rest, stderr)
	case "healthcheck":
		err = runHealthcheck(ctx, rest, environ, stderr)
	case "help", "-h", "--help":
		_, _ = io.WriteString(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "koro: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		_, _ = fmt.Fprintf(stderr, "koro %s: %v\n", cmd, err)
		return 2
	default:
		// Configuration errors name variables, never values (05 §12.2).
		_, _ = fmt.Fprintf(stderr, "koro %s: %v\n", cmd, err)
		return 1
	}
}
