package db_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

type event struct {
	ID    uuid.UUID `gorm:"type:uuid;primaryKey"`
	OrgID uuid.UUID `gorm:"type:uuid"`
	Title string
}

func (event) TableName() string { return "events" }

// dryRun returns a GORM handle that only builds SQL (no connection is made).
func dryRun(t *testing.T) *gorm.DB {
	t.Helper()
	orm, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=127.0.0.1 port=1 user=nobody dbname=none"}),
		&gorm.Config{DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatal(err)
	}
	return orm
}

func TestScopeOrg_FiltersAndRejectsNilOrg(t *testing.T) {
	t.Parallel()
	orm := dryRun(t)
	org, id := uuid.NewV7(), uuid.NewV7()

	var e event
	stmt := orm.Scopes(db.ScopeOrg(org)).Where("id = ?", id).Take(&e).Statement
	if got := stmt.SQL.String(); got != `SELECT * FROM "events" WHERE id = $1 AND "events"."org_id" = $2 LIMIT $3` {
		t.Fatalf("SQL = %s", got)
	}
	if len(stmt.Vars) < 2 || stmt.Vars[1] != org {
		t.Fatalf("org parameter = %v", stmt.Vars)
	}

	for name, run := range map[string]func(*gorm.DB) *gorm.DB{
		"take":   func(tx *gorm.DB) *gorm.DB { return tx.Take(&event{}) },
		"find":   func(tx *gorm.DB) *gorm.DB { var es []event; return tx.Find(&es) },
		"update": func(tx *gorm.DB) *gorm.DB { return tx.Model(&event{}).Where("id = ?", id).Update("title", "x") },
		"delete": func(tx *gorm.DB) *gorm.DB { return tx.Where("id = ?", id).Delete(&event{}) },
		"create": func(tx *gorm.DB) *gorm.DB { return tx.Create(&event{ID: id}) },
	} {
		res := run(orm.Scopes(db.ScopeOrg(uuid.Nil())))
		if !errors.Is(res.Error, db.ErrMissingOrgScope) {
			t.Errorf("%s with nil org: error = %v, want ErrMissingOrgScope", name, res.Error)
		}
		if sql := res.Statement.SQL.String(); sql != "" {
			t.Errorf("%s with nil org built SQL %q", name, sql)
		}
	}
}

func pgErr(code, constraint string) error {
	return fmt.Errorf("store: %w", &pgconn.PgError{Code: code, ConstraintName: constraint, Message: "boom"})
}

var errSlugTaken = problem.New(problem.Conflict, "dbtest.slug_taken", "Slug taken")

func TestTranslate_MapsToProblemKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want *problem.Error // nil: returned unchanged
	}{
		{"gorm not found", gorm.ErrRecordNotFound, problem.ErrNotFound},
		{"pgx no rows", fmt.Errorf("get: %w", pgx.ErrNoRows), problem.ErrNotFound},
		{"unique", pgErr(db.CodeUniqueViolation, "t_x_uq"), problem.ErrConflict},
		{"exclusion", pgErr(db.CodeExclusionViolation, "t_range_excl"), problem.ErrConflict},
		{"foreign key", pgErr(db.CodeForeignKeyViolation, "t_parent_fkey"), problem.ErrConflict},
		{"serialization", pgErr(db.CodeSerializationFailure, ""), problem.ErrConflict},
		{"deadlock", pgErr(db.CodeDeadlockDetected, ""), problem.ErrConflict},
		{"check", pgErr(db.CodeCheckViolation, "t_x_chk"), problem.ErrValidation},
		{"not null", pgErr("23502", ""), nil},
		{"immutable", pgErr(db.CodeRestrictViolation, ""), nil},
		{"lock timeout", pgErr("55P03", ""), nil},
		{"statement timeout", pgErr("57014", ""), nil},
		{"missing scope", db.ErrMissingOrgScope, nil},
		{"plain", errors.New("network"), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := db.Translate(tt.err)
			if tt.want == nil {
				if got != tt.err { //nolint:errorlint // identity: the error must be returned unchanged
					t.Fatalf("Translate() = %v, want the input unchanged", got)
				}
				return
			}
			if !errors.Is(got, tt.want) {
				t.Fatalf("Translate() = %v, want %s", got, tt.want.Code())
			}
			if !errors.Is(got, tt.err) {
				t.Fatalf("Translate() lost the cause: %v", got)
			}
		})
	}
	if db.Translate(nil) != nil {
		t.Fatal("Translate(nil) != nil")
	}
	if p := problem.ErrForbidden.Wrap(pgErr(db.CodeUniqueViolation, "")); db.Translate(p) != p { //nolint:errorlint // identity
		t.Fatal("a *problem.Error must pass through unchanged")
	}

	c := db.Constraints{"events_org_slug_uq": errSlugTaken}
	if got := c.Translate(pgErr(db.CodeUniqueViolation, "events_org_slug_uq")); !errors.Is(got, errSlugTaken) || db.ConstraintName(got) != "events_org_slug_uq" {
		t.Fatalf("Constraints.Translate(listed) = %v", got)
	}
	// A generic translation done earlier (WithTx commit) is refined too.
	if got := c.Translate(db.Translate(pgErr(db.CodeUniqueViolation, "events_org_slug_uq"))); !errors.Is(got, errSlugTaken) {
		t.Fatalf("Constraints.Translate(translated) = %v", got)
	}
	if got := c.Translate(pgErr(db.CodeUniqueViolation, "other_uq")); !errors.Is(got, problem.ErrConflict) {
		t.Fatalf("Constraints.Translate(unlisted) = %v", got)
	}
	if !db.IsRetryable(pgErr(db.CodeSerializationFailure, "")) || db.IsRetryable(pgErr(db.CodeUniqueViolation, "")) {
		t.Fatal("IsRetryable")
	}
}

func TestAfterCommit_WithoutTxRunsImmediately(t *testing.T) {
	t.Parallel()
	ran := false
	db.AfterCommit(t.Context(), func(context.Context) { ran = true })
	if !ran {
		t.Fatal("AfterCommit without a transaction must run at once")
	}
}

func TestNopTx_JoinsAndDefersHooks(t *testing.T) {
	t.Parallel()
	var tx db.TxManager = db.NopTx{}
	var log []string
	err := tx.WithTx(t.Context(), func(ctx context.Context) error {
		if !db.InTx(ctx) {
			t.Error("InTx = false inside NopTx")
		}
		db.AfterCommit(ctx, func(context.Context) { log = append(log, "outer") })
		return tx.WithTx(ctx, func(ctx context.Context) error {
			db.AfterCommit(ctx, func(context.Context) { log = append(log, "inner") })
			log = append(log, "body")
			return nil
		})
	})
	if err != nil || strings.Join(log, ",") != "body,outer,inner" {
		t.Fatalf("log = %v, err = %v", log, err)
	}

	log = nil
	errBoom := errors.New("boom")
	err = tx.WithSerializableTx(t.Context(), func(ctx context.Context) error {
		db.AfterCommit(ctx, func(context.Context) { log = append(log, "hook") })
		return errBoom
	})
	if !errors.Is(err, errBoom) || len(log) != 0 {
		t.Fatalf("failed NopTx ran hooks %v (err %v)", log, err)
	}
}

func TestOpen_InvalidURLDoesNotLeakPassword(t *testing.T) {
	t.Parallel()
	_, err := db.Open(t.Context(), db.Config{URL: "postgres://app_rw:s3cr3t-pw@host:notaport/koro"}, obs.Discard())
	if err == nil || strings.Contains(err.Error(), "s3cr3t-pw") {
		t.Fatalf("Open() error = %v", err)
	}
}
