//go:build integration

package identity_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/identity"
	"koro/internal/platform/testutil"
)

func TestEduStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	eduStoreContract(t, gormEduWorld)
}

func TestRetentionStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	retentionContract(t, gormEduWorld)
}

// gormEduWorld is a fresh database with university and file factories.
func gormEduWorld(t *testing.T) eduWorld {
	d := testutil.NewDB(t)
	s := identity.NewGormStore(d.ORM)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return eduWorld{
		s: s,
		// The database stamps a new row now (column defaults); a row of a
		// known age is backdated by the table owner with the touch trigger
		// off for that one statement (test only: the application never
		// writes created_at/updated_at, BE-069).
		insertAt: func(t *testing.T, v identity.EduRecord, at time.Time) {
			t.Helper()
			ctx := t.Context()
			if err := s.InsertEduVerification(ctx, v); err != nil {
				t.Fatal(err)
			}
			tx, err := testutil.OwnerConn(t, d).Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`ALTER TABLE edu_verifications DISABLE TRIGGER edu_verifications_touch_updated_at`, nil},
				{`UPDATE edu_verifications SET created_at = $2, updated_at = $2 WHERE id = $1`, []any{v.ID, at}},
				{`ALTER TABLE edu_verifications ENABLE TRIGGER edu_verifications_touch_updated_at`, nil},
			} {
				if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
					t.Fatalf("%s: %v", q.sql, err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		},
		uni: func(t *testing.T) uuid.UUID {
			org, uni := uuid.NewV7(), uuid.NewV7()
			exec(`INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'university', $2, $3)`, org, "Üni "+org.String(), "uni-"+org.String())
			exec(`INSERT INTO universities (id, org_id, yok_code, name, city, type) VALUES ($1, $2, $3, $4, 'Ankara', 'state')`,
				uni, org, "Y"+uni.String()[:8], "Üniversite "+uni.String())
			return uni
		},
		file: func(t *testing.T, owner uuid.UUID) uuid.UUID {
			id := uuid.NewV7()
			exec(`INSERT INTO files (id, purpose, owner_user_id, uploaded_by, bucket, storage_key, declared_mime)
				VALUES ($1, 'edu_document', $2, $2, 'koro-files-dev', $3, 'application/pdf')`, id, owner, "edu_document/2026/10/"+id.String())
			return id
		},
	}
}
