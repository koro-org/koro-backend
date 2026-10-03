// Package testutil provides the integration test environment (05 §13.4):
// one PostgreSQL 18 and one Redis 8 container per test binary, started by
// TestMain:
//
//	//go:build integration
//
//	func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }
//
// PostgreSQL is initialised like production and deploy/compose.dev.yaml:
// ICU tr-TR initdb arguments, deploy/db/bootstrap.sql roles (app_owner is
// not a superuser), deploy/db/dev-passwords.sql. Every migration is applied
// once, as app_owner, to the template database koro_tmpl; NewDB clones it
// into an isolated database per test and connects as app_rw with the
// production pool settings (QueryExecModeExec). Tests call t.Parallel(); the
// containers are the only shared state.
//
// Container start-up, the first connections and per-test database
// creation are retried with exponential backoff (Retry, Transient; 06 H-36):
// under parallel Docker load a fresh container may drop its first
// connection ("unexpected EOF") or the reaper may time out.
//
// Everything except this file and retry.go needs the integration build tag;
// synthetic data generators live in the untagged subpackage fake.
package testutil
