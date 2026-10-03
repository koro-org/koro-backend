//go:build integration

package identity_test

import (
	"testing"
	"uuid"

	"koro/internal/identity"
	"koro/internal/platform/testutil"
)

func TestEduStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	eduStoreContract(t, func(t *testing.T) eduWorld {
		d := testutil.NewDB(t)
		exec := func(sql string, args ...any) {
			t.Helper()
			if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		return eduWorld{
			s: identity.NewGormStore(d.ORM),
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
	})
}
