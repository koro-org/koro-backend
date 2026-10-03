//go:build integration

package migrations_test

import (
	"crypto/sha256"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// Test helpers of the P1 schema-1 tests (migrations 000010–000043: consent,
// identity, notify, orgs, directory). Every test builds the rows it needs
// through the app_rw pool, i.e. with the privileges of the application, so
// a missing GRANT shows up as a failing fixture.

// SQLSTATEs the tests expect besides the db.Code* constants.
const (
	codeNotNull     = "23502"
	codeObjectState = "55000" // cannot modify a view
)

// p1aWorld creates the rows the constraint tests refer to.
type p1aWorld struct {
	t   *testing.T
	d   *db.DB
	seq atomic.Int64
}

func newP1aWorld(t *testing.T) *p1aWorld {
	t.Helper()
	return &p1aWorld{t: t, d: testutil.NewDB(t)}
}

// n returns a number no other call of this world returned (unique slugs,
// e-mail addresses, codes).
func (w *p1aWorld) n() string { return strconv.FormatInt(w.seq.Add(1), 10) }

// exec runs sql as app_rw and fails the test on error.
func (w *p1aWorld) exec(sql string, args ...any) {
	w.t.Helper()
	if _, err := w.d.Pool.Exec(w.t.Context(), sql, args...); err != nil {
		w.t.Fatalf("%s: %v", sql, err)
	}
}

// count returns the result of a "SELECT count(*) ..." query.
func (w *p1aWorld) count(sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.d.Pool.QueryRow(w.t.Context(), sql, args...).Scan(&n); err != nil {
		w.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// user inserts an active, non-student user and returns its id.
func (w *p1aWorld) user() uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
		VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, "u"+w.n()+"@example.test")
	return id
}

// org inserts an active org of the given kind (not 'platform': that one is
// seeded by 000031) and returns its id.
func (w *p1aWorld) org(kind string) uuid.UUID {
	w.t.Helper()
	id, n := uuid.NewV7(), w.n()
	w.exec(`INSERT INTO organizations (id, kind, name, slug) VALUES ($1, $2, $3, $4)`, id, kind, "Org "+n, "org-"+n)
	return id
}

// platformOrg returns the seeded platform org.
func (w *p1aWorld) platformOrg() uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.d.Pool.QueryRow(w.t.Context(), `SELECT id FROM organizations WHERE kind = 'platform'`).Scan(&id); err != nil {
		w.t.Fatalf("platform org: %v", err)
	}
	return id
}

// university inserts an org of kind 'university' with its universities row
// and returns (university id, org id).
func (w *p1aWorld) university() (uniID, orgID uuid.UUID) {
	w.t.Helper()
	orgID, uniID = w.org("university"), uuid.NewV7()
	w.exec(`INSERT INTO universities (id, org_id, yok_code, name, city, type) VALUES ($1, $2, $3, $4, 'Ankara', 'state')`,
		uniID, orgID, "YOK"+w.n(), "Üniversite "+w.n())
	return uniID, orgID
}

// club inserts an org of kind 'club' with its clubs row at the university
// and returns the club org id.
func (w *p1aWorld) club(universityID uuid.UUID) uuid.UUID {
	w.t.Helper()
	orgID := w.org("club")
	w.exec(`INSERT INTO clubs (org_id, university_id, category) VALUES ($1, $2, 'academic')`, orgID, universityID)
	return orgID
}

// file inserts an uploaded-file row (files.purpose = purpose).
func (w *p1aWorld) file(purpose string) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO files (id, purpose, bucket, storage_key, declared_mime) VALUES ($1, $2, 'koro-files-dev', $3, 'application/pdf')`,
		id, purpose, purpose+"/2026/10/"+id.String())
	return id
}

// notice inserts a published consent_text version for purpose and returns
// its id; version must differ for two notices of one purpose.
func (w *p1aWorld) notice(purpose, version string) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO notice_versions (id, kind, purpose_code, audience, version, title, body_md, body_sha256, published_at)
		VALUES ($1, 'consent_text', $2, 'all', $3, 'Açık rıza metni', 'Metin', $4, now())`, id, purpose, version, sha("Metin"))
	return id
}

// sha returns the SHA-256 of s.
func sha(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// bytes32 returns a 32-byte value that differs for every n (hash columns).
func bytes32(n string) []byte { return sha("p1a-" + n) }

// p1aCase is one statement that must fail with the given SQLSTATE and, when
// constraint is set, with that explicit constraint (or index) name.
type p1aCase struct {
	name       string
	sql        string
	args       []any
	state      string
	constraint string
}

// expectFail runs the cases (each on its own, nothing is left behind by a
// failing statement) and reports every case that does not fail as expected.
func expectFail(t *testing.T, q execer, cases []p1aCase) {
	t.Helper()
	for _, c := range cases {
		_, err := q.Exec(t.Context(), c.sql, c.args...)
		if err == nil {
			t.Errorf("%s: succeeded, want SQLSTATE %s %s", c.name, c.state, c.constraint)
			continue
		}
		if got := db.SQLState(err); got != c.state {
			t.Errorf("%s: SQLSTATE %q, want %s (%v)", c.name, got, c.state, err)
			continue
		}
		if c.constraint != "" {
			if got := db.ConstraintName(err); got != c.constraint {
				t.Errorf("%s: constraint %q, want %q (%v)", c.name, got, c.constraint, err)
			}
		}
	}
}

// expectOK runs statements that must succeed.
func expectOK(t *testing.T, q execer, cases []p1aCase) {
	t.Helper()
	for _, c := range cases {
		if _, err := q.Exec(t.Context(), c.sql, c.args...); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// repeat returns s repeated n times (over-long column values).
func repeat(s string, n int) string { return strings.Repeat(s, n) }
