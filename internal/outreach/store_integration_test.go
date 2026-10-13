//go:build integration

package outreach_test

import (
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var seq atomic.Int64

// pgWorld inserts the parent rows (organizations, users) as app_rw.
func pgWorld(d *db.DB) world {
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return world{
		org: func(t *testing.T) uuid.UUID {
			id, n := uuid.NewV7(), strconv.FormatInt(seq.Add(1), 10)
			exec(t, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'company', $2, $3)`, id, "Şirket "+n, "sirket-"+n)
			return id
		},
		user: func(t *testing.T) uuid.UUID {
			id := uuid.NewV7()
			exec(t, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
				VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, "u"+strconv.FormatInt(seq.Add(1), 10)+"@example.test")
			return id
		},
		// A company's public post (the dispatch FK; the audience does not matter to the store).
		post: func(t *testing.T, org, by uuid.UUID) uuid.UUID {
			id := uuid.NewV7()
			exec(t, `INSERT INTO posts (id, org_id, author_org_kind, author_user_id, kind, title, body_json, audience_kind,
				audience_keys, audience_dim_count, commercial_flag)
				VALUES ($1, $2, 'company', $3, 'text', 'Yaz stajı', '{"type":"doc"}', 'public', ARRAY['all'], 1, true)`,
				id, org, by)
			return id
		},
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (outreach.Store, world) {
		d := testutil.NewDB(t)
		return outreach.NewGormStore(d.ORM), pgWorld(d)
	})
}

// TestSegment_Postgres_LifecycleAndRecount runs the service over
// PostgreSQL (READ COMMITTED transactions of the app_rw pool): the jsonb
// definition and text[] keys round-trip, updated_at comes from the
// segments_touch_updated_at trigger (the ETag follows it), the CHECK
// refuses a last_count below 10, and the recount job writes in its
// transaction.
func TestSegment_Postgres_LifecycleAndRecount(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	e := newEnvWith(t, clk, outreach.NewGormStore(d.ORM), d.Tx, idempotency.NewGormStore(d.ORM))
	ctx := t.Context()

	// The personas' org (company_a) and user must exist for the FKs.
	cma := actor(t, "company_admin")
	if _, err := d.Pool.Exec(ctx, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'company', 'Şirket A', 'sirket-a')`, companyA); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
		VALUES ($1, 'active', 'cma@example.test', 'cma@example.test', 'Ada', 'Yılmaz', 'company')`, cma.UserID); err != nil {
		t.Fatal(err)
	}
	e.orgs.set(companyA, orgs.KindCompany, orgs.StatusActive)
	e.audience.add(64, true, uniKey(uniA), yearKey(3))

	def := outreach.Definition{Uni: []uuid.UUID{uniA}, Year: []int16{3}}
	seg, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "ODTÜ üçüncü sınıf", Definition: def})
	if err != nil {
		t.Fatal(err)
	}
	if seg.Estimate.Count == nil || *seg.Estimate.Count != 60 || seg.ETag == "" {
		t.Fatalf("created %+v", seg)
	}
	var stored int
	if err := d.Pool.QueryRow(ctx, `SELECT last_count FROM segments WHERE id = $1`, seg.ID).Scan(&stored); err != nil || stored != 60 {
		t.Fatalf("stored last_count = %d, %v (the exact 64 is never stored)", stored, err)
	}

	name := "Yeni ad"
	upd, err := e.svc.UpdateSegment(ctx, cma, companyA, seg.ID, seg.ETag, outreach.SegmentPatch{Name: &name})
	if err != nil || upd.Name != name || upd.ETag == seg.ETag {
		t.Fatalf("update = %+v, %v", upd, err)
	}
	if _, err := e.svc.UpdateSegment(ctx, cma, companyA, seg.ID, seg.ETag, outreach.SegmentPatch{Name: &name}); err == nil {
		t.Fatal("stale ETag accepted")
	}

	// The CHECK of PostgreSQL itself.
	if _, err := d.Pool.Exec(ctx, `UPDATE segments SET last_count = 9 WHERE id = $1`, seg.ID); err == nil {
		t.Fatal("PostgreSQL accepted last_count 9")
	}

	// Recount: the audience shrank below 10.
	e.audience.reset()
	e.audience.add(4, true, uniKey(uniA), yearKey(3))
	e.recount(t)
	got, err := e.svc.GetSegment(ctx, cma, companyA, seg.ID)
	if err != nil || got.Estimate.Count != nil || !got.Estimate.BelowThreshold || got.ETag == upd.ETag {
		t.Fatalf("after recount %+v, %v", got, err)
	}
}
