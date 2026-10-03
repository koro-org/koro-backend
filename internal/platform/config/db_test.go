package config_test

import (
	"strings"
	"testing"
	"time"

	"koro/internal/platform/config"
)

func TestConfig_DB_DefaultsAndValidation(t *testing.T) {
	t.Parallel()
	cfg, err := config.LoadAPI(validAPIEnv())
	if err != nil {
		t.Fatalf("LoadAPI() error = %v", err)
	}
	d := cfg.Platform.DB
	if d.MaxConns != 20 || d.MinConns != 2 || d.StatementTimeout != 15*time.Second || d.IdleInTxTimeout != 30*time.Second {
		t.Fatalf("unexpected DB defaults: %+v", d)
	}
	if !strings.HasPrefix(d.URL.Reveal(), "postgres://app_rw:") {
		t.Fatalf("DATABASE_URL not loaded")
	}

	const secretURL = "postgres://app_owner:s3cr3t-pw@db:5432/koro"
	bad := []struct {
		name    string
		environ map[string]string
		want    string
	}{
		{"missing url", validAPIEnv("DATABASE_URL", ""), `"DATABASE_URL" should not be empty`},
		{"not a url", validAPIEnv("DATABASE_URL", "host=db password=s3cr3t-pw"), "DATABASE_URL must be a postgres://"},
		{"mysql", validAPIEnv("DATABASE_URL", "mysql://u:s3cr3t-pw@db/koro"), "DATABASE_URL must be a postgres://"},
		{"no database", validAPIEnv("DATABASE_URL", "postgres://app_rw:s3cr3t-pw@db:5432"), "DATABASE_URL must name a database"},
		{"owner role", validAPIEnv("DATABASE_URL", secretURL), "not the owner role app_owner"},
		{"zero max", validAPIEnv("DB_MAX_CONNS", "0"), "DB_MAX_CONNS"},
		{"min above max", validAPIEnv("DB_MAX_CONNS", "2", "DB_MIN_CONNS", "3"), "DB_MIN_CONNS"},
		{"zero statement timeout", validAPIEnv("DB_STATEMENT_TIMEOUT", "0s"), "DB_STATEMENT_TIMEOUT"},
		{"zero idle timeout", validAPIEnv("DB_IDLE_IN_TX_TIMEOUT", "0s"), "DB_IDLE_IN_TX_TIMEOUT"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.LoadAPI(tt.environ)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadAPI() error = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "s3cr3t-pw") {
				t.Fatalf("error leaks the database password: %v", err)
			}
		})
	}
}

func TestConfig_LoadMigrate(t *testing.T) {
	t.Parallel()
	const url = "postgres://app_owner:s3cr3t-pw@localhost:5432/koro?sslmode=disable"
	cfg, err := config.LoadMigrate(map[string]string{"APP_ENV": "dev", "MIGRATE_DATABASE_URL": url})
	if err != nil {
		t.Fatalf("LoadMigrate() error = %v", err)
	}
	if cfg.Migratekit.URL.Reveal() != url {
		t.Fatal("MIGRATE_DATABASE_URL not loaded")
	}
	for _, environ := range []map[string]string{
		{"APP_ENV": "dev"},
		{"APP_ENV": "dev", "MIGRATE_DATABASE_URL": "app_owner:s3cr3t-pw@localhost"},
	} {
		_, err := config.LoadMigrate(environ)
		if err == nil || !strings.Contains(err.Error(), "MIGRATE_DATABASE_URL") {
			t.Fatalf("LoadMigrate(%v) error = %v, want MIGRATE_DATABASE_URL error", len(environ), err)
		}
		if strings.Contains(err.Error(), "s3cr3t-pw") {
			t.Fatalf("error leaks the database password: %v", err)
		}
	}
}
