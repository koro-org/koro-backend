//go:build integration

package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"koro/db/migrations"
	"koro/internal/platform/db"
	"koro/internal/platform/migratekit"
	"koro/internal/platform/obs"
)

// Images and settings (R-002 §3h; 02 §0.3).
const (
	PostgresImage = "postgres:18-alpine"
	RedisImage    = "redis:8-alpine"
	TemplateDB    = "koro_tmpl"

	// RolePassword is the password of app_owner/app_rw/app_ro set by
	// deploy/db/dev-passwords.sql (dev and tests only).
	RolePassword = "dev-only-change-me" //nolint:gosec // G101: dev/test-only placeholder, not a secret

	initdbArgs    = "--encoding=UTF8 --locale-provider=icu --icu-locale=tr-TR --locale=C.UTF-8"
	superUser     = "postgres"
	superPassword = "test-only-superuser"
	setupTimeout  = 5 * time.Minute
	redisDBs      = 15 // logical DBs 1..15 (0 is never handed out)

	// readyTimeout bounds one container start (both "ready" log lines of
	// PostgreSQL and the mapped port); the default minute is too short when
	// several test binaries start containers at once (06 H-36).
	readyTimeout = 2 * time.Minute
	// ryukTimeout is the reaper connection timeout (default 1 m), set
	// through the testcontainers environment unless the caller chose one.
	ryukTimeout = "3m"
)

// environment is the per-binary shared state built by Run.
type environment struct {
	pg     *tcpostgres.PostgresContainer
	host   string
	port   string
	admin  *pgxpool.Pool // superuser, database "postgres"
	latest uint          // newest embedded migration

	redisOnce sync.Once
	redisCtr  *testcontainers.DockerContainer
	redisAddr string
	redisErr  error
	redisDBs  chan int
}

var env *environment

// Run starts PostgreSQL, prepares the template database, runs the tests and
// terminates the containers. Use it as os.Exit(testutil.Run(m)).
func Run(m *testing.M) int {
	for _, k := range []string{"TESTCONTAINERS_RYUK_CONNECTION_TIMEOUT", "TESTCONTAINERS_RYUK_RECONNECTION_TIMEOUT"} {
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, ryukTimeout)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	e, err := setup(ctx)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testutil: setup: %v\n", err)
		if e != nil {
			e.teardown()
		}
		return 1
	}
	env = e
	code := m.Run()
	e.teardown()
	return code
}

func setup(ctx context.Context) (*environment, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	e := &environment{redisDBs: make(chan int, redisDBs)}
	for i := 1; i <= redisDBs; i++ {
		e.redisDBs <- i
	}
	e.latest, err = migratekit.Latest(migrations.FS)
	if err != nil {
		return nil, err
	}

	// Container start, first connection and migration are retried with
	// exponential backoff (06 H-36): a start that times out is terminated
	// and started again; a dropped first connection is simply re-dialled.
	err = Retry(ctx, setupBackoff, "start postgres", nil, func(ctx context.Context) error {
		pg, err := tcpostgres.Run(ctx, PostgresImage,
			tcpostgres.WithDatabase(TemplateDB),
			tcpostgres.WithUsername(superUser),
			tcpostgres.WithPassword(superPassword),
			testcontainers.WithEnv(map[string]string{"POSTGRES_INITDB_ARGS": initdbArgs}),
			tcpostgres.WithOrderedInitScripts(
				filepath.Join(root, "deploy", "db", "bootstrap.sql"),
				filepath.Join(root, "deploy", "db", "dev-passwords.sql"),
			),
			testcontainers.WithCmd("postgres",
				"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off",
				"-c", "max_connections=300"),
			// tcpostgres.BasicWaitStrategies with a longer start-up timeout.
			testcontainers.WithAdditionalWaitStrategy(
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(readyTimeout),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(readyTimeout),
			),
		)
		if err != nil {
			if pg != nil {
				_ = pg.Terminate(context.WithoutCancel(ctx))
			}
			return err
		}
		e.pg = pg
		return nil
	})
	if err != nil {
		return e, err
	}
	e.host, err = e.pg.Host(ctx)
	if err != nil {
		return e, fmt.Errorf("postgres host: %w", err)
	}
	p, err := e.pg.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return e, fmt.Errorf("postgres port: %w", err)
	}
	e.port = strconv.Itoa(int(p.Num()))

	e.admin, err = pgxpool.New(ctx, e.url(superUser, superPassword, "postgres"))
	if err != nil {
		return e, fmt.Errorf("admin pool: %w", err)
	}
	if err := Retry(ctx, setupBackoff, "admin connection", Transient, e.admin.Ping); err != nil {
		return e, err
	}

	// Apply every migration once, as the (non-superuser) owner role. A
	// migration that fails half way leaves a dirty version, so only the
	// connection (before Up) is retried.
	var mig *migratekit.Migrator
	err = Retry(ctx, setupBackoff, "connect migrator", Transient, func(context.Context) error {
		var err error
		mig, err = migratekit.New(migrations.FS, e.url("app_owner", RolePassword, TemplateDB), obs.Discard())
		return err
	})
	if err != nil {
		return e, err
	}
	upErr := mig.Up(ctx)
	if err := errors.Join(upErr, mig.Close()); err != nil {
		return e, fmt.Errorf("migrate %s: %w", TemplateDB, err)
	}
	// A template must have no sessions while it is cloned.
	if _, err := e.admin.Exec(ctx, `ALTER DATABASE `+TemplateDB+` WITH ALLOW_CONNECTIONS false`); err != nil {
		return e, fmt.Errorf("lock template: %w", err)
	}
	return e, nil
}

func (e *environment) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if e.admin != nil {
		e.admin.Close()
	}
	if e.pg != nil {
		_ = e.pg.Terminate(ctx)
	}
	if e.redisCtr != nil {
		_ = e.redisCtr.Terminate(ctx)
	}
}

func (e *environment) url(user, password, database string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(e.host, e.port),
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// repoRoot finds the module root (go.mod) above the working directory of
// the test binary (its package directory).
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("testutil: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("testutil: go.mod not found above the working directory")
		}
		dir = parent
	}
}

func mustEnv(t testing.TB) *environment {
	t.Helper()
	if env == nil {
		t.Fatal("testutil: Run was not called (TestMain: os.Exit(testutil.Run(m)))")
	}
	return env
}

func randomName(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// createDB creates database name owned by app_owner from template and drops
// it (FORCE) when the test ends.
func createDB(t testing.TB, template string) string {
	t.Helper()
	e := mustEnv(t)
	name := randomName("t_")
	ident := pgx.Identifier{name}.Sanitize()
	err := Retry(t.Context(), setupBackoff, "create database", Transient, func(ctx context.Context) error {
		_, err := e.admin.Exec(ctx, `CREATE DATABASE `+ident+` OWNER app_owner TEMPLATE `+template)
		return err
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := e.admin.Exec(ctx, `DROP DATABASE IF EXISTS `+ident+` WITH (FORCE)`); err != nil {
			t.Errorf("testutil: drop database: %v", err)
		}
	})
	return name
}

// NewDB returns an isolated, fully migrated database connected as app_rw
// with the production pool settings (QueryExecModeExec, statement and
// idle-in-transaction timeouts, UTC). It is dropped when the test ends.
func NewDB(t testing.TB) *db.DB {
	t.Helper()
	e := mustEnv(t)
	name := createDB(t, TemplateDB)
	var d *db.DB
	err := Retry(t.Context(), setupBackoff, "open database", Transient, func(ctx context.Context) error {
		var err error
		d, err = db.Open(ctx, db.Config{
			URL:              e.url("app_rw", RolePassword, name),
			AppName:          "koro-test",
			MaxConns:         4,
			MinConns:         0,
			StatementTimeout: 15 * time.Second,
			IdleInTxTimeout:  30 * time.Second,
		}, obs.Discard())
		return err
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if err := installQueryCounter(d.ORM); err != nil {
		d.Close()
		t.Fatalf("testutil: %v", err)
	}
	t.Cleanup(d.Close) // runs before the DROP registered by createDB
	return d
}

// NewEmptyDB returns the owner (app_owner) URL of an isolated database
// without any migration, for migration tests. It is dropped when the test
// ends.
func NewEmptyDB(t testing.TB) string {
	t.Helper()
	return mustEnv(t).url("app_owner", RolePassword, createDB(t, "template0"))
}

// OwnerURL returns the app_owner URL of d's database.
func OwnerURL(t testing.TB, d *db.DB) string {
	t.Helper()
	return mustEnv(t).url("app_owner", RolePassword, d.Pool.Config().ConnConfig.Database)
}

// AppURL returns the app_rw URL of d's database (DATABASE_URL of a test
// process).
func AppURL(t testing.TB, d *db.DB) string {
	t.Helper()
	return mustEnv(t).url("app_rw", RolePassword, d.Pool.Config().ConnConfig.Database)
}

// OwnerConn opens a connection to d's database as app_owner, for tests that
// need DDL or must look past the application role's grants. It is closed
// when the test ends.
func OwnerConn(t testing.TB, d *db.DB) *pgx.Conn {
	t.Helper()
	var c *pgx.Conn
	err := Retry(t.Context(), setupBackoff, "owner connection", Transient, func(ctx context.Context) error {
		var err error
		c, err = pgx.Connect(ctx, OwnerURL(t, d))
		return err
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// LatestMigration returns the newest migration embedded in the binary.
func LatestMigration(t testing.TB) uint {
	t.Helper()
	return mustEnv(t).latest
}

// NewRedis returns a client on an empty logical Redis database (1–15) of
// the shared Redis container, flushed again and released when the test
// ends. The container starts on first use. With all 15 databases in use it
// waits for one to be released.
func NewRedis(t testing.TB) *redis.Client {
	t.Helper()
	e := mustEnv(t)
	e.redisOnce.Do(e.startRedis)
	if e.redisErr != nil {
		t.Fatalf("testutil: redis: %v", e.redisErr)
	}
	var idx int
	select {
	case idx = <-e.redisDBs:
	case <-t.Context().Done():
		t.Fatal("testutil: no free redis database")
	}
	c := redis.NewClient(&redis.Options{Addr: e.redisAddr, DB: idx})
	if err := c.FlushDB(t.Context()).Err(); err != nil {
		e.redisDBs <- idx
		t.Fatalf("testutil: redis flush: %v", err)
	}
	t.Cleanup(func() {
		_ = c.FlushDB(context.Background()).Err()
		_ = c.Close()
		e.redisDBs <- idx
	})
	return c
}

func (e *environment) startRedis() {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	var ctr *testcontainers.DockerContainer
	err := Retry(ctx, setupBackoff, "start redis", nil, func(ctx context.Context) error {
		c, err := testcontainers.Run(ctx, RedisImage,
			testcontainers.WithExposedPorts("6379/tcp"),
			// asynq requirements (R-002 §5.3).
			testcontainers.WithCmd("redis-server", "--appendonly", "yes", "--maxmemory-policy", "noeviction"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("Ready to accept connections").WithStartupTimeout(readyTimeout),
				wait.ForListeningPort("6379/tcp").WithStartupTimeout(readyTimeout),
			),
		)
		if err != nil {
			if c != nil {
				_ = c.Terminate(context.WithoutCancel(ctx))
			}
			return err
		}
		ctr = c
		return nil
	})
	e.redisCtr = ctr
	if err != nil {
		e.redisErr = err
		return
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		e.redisErr = err
		return
	}
	p, err := ctr.MappedPort(ctx, "6379/tcp")
	if err != nil {
		e.redisErr = err
		return
	}
	e.redisAddr = net.JoinHostPort(host, strconv.Itoa(int(p.Num())))
}
