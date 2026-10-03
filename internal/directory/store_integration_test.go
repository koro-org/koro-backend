//go:build integration

package directory_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"uuid"

	"koro/db/reference"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/identity"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// pgUser inserts a user as app_rw (FK targets of verified_by, set_by).
func pgUser(d *db.DB) func(t *testing.T) uuid.UUID {
	return func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.NewV7()
		email := "u" + strconv.FormatInt(seq.Add(1), 10) + "@example.test"
		if _, err := d.Pool.Exec(t.Context(), `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
			VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, email); err != nil {
			t.Fatal(err)
		}
		return id
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (directory.Store, world) {
		d := testutil.NewDB(t)
		return directory.NewGormStore(d.ORM), orgWorld(orgs.NewGormStore(d.ORM), pgUser(d))
	})
}

// pgEnv is the directory module over PostgreSQL with the real orgs module.
type pgEnv struct {
	d     *db.DB
	store directory.Store
	orgs  orgs.Service
	svc   directory.Service
	audit *auditLog
}

func newPGEnv(t *testing.T) pgEnv {
	t.Helper()
	d := testutil.NewDB(t)
	clk := clock.NewFake(t0)
	log := obs.Discard()
	out := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: outbox.NewRegistry(), Queue: queue.NewMem(), Clock: clk, Log: log})
	idem := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: log})
	al := &auditLog{}
	om, err := orgs.New(orgs.Deps{Store: orgs.NewGormStore(d.ORM), Tx: d.Tx, Outbox: out, Audit: al, Idempotency: idem, Clock: clk, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	store := directory.NewGormStore(d.ORM)
	mod, err := directory.New(directory.Deps{
		Store: store, Tx: d.Tx, Outbox: out, Queue: queue.NewMem(), Audit: al, Orgs: om.Service, Idempotency: idem,
		Clock: clk, Log: log, Reauth: reauth{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return pgEnv{d: d, store: store, orgs: om.Service, svc: mod.Service, audit: al}
}

// count runs SELECT count(*) with args.
func (e pgEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.d.Pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSeedReference_Idempotent_Gorm imports the embedded reference data twice
// into PostgreSQL: the second run changes nothing (rows, orgs).
func TestSeedReference_Idempotent_Gorm(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t)
	ctx := t.Context()
	data, err := directory.ParseReference(reference.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ImportReference(ctx, data); err != nil {
		t.Fatal(err)
	}
	tables := []string{"universities", "university_email_domains", "faculties", "departments", "skills"}
	before := map[string]int{}
	for _, tb := range tables {
		before[tb] = e.count(t, "SELECT count(*) FROM "+tb)
	}
	orgsBefore := e.count(t, "SELECT count(*) FROM organizations WHERE kind = 'university'")
	res, err := e.svc.ImportReference(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if res.Universities.Created+res.Universities.Updated+res.Domains.Created+res.Domains.Updated+res.Faculties.Created+
		res.Faculties.Updated+res.Departments.Created+res.Departments.Updated+res.Skills.Created+res.Skills.Updated != 0 {
		t.Fatalf("second run changed rows: %+v", res)
	}
	for _, tb := range tables {
		if n := e.count(t, "SELECT count(*) FROM "+tb); n != before[tb] {
			t.Fatalf("%s: %d rows after the re-import, %d before", tb, n, before[tb])
		}
	}
	if n := e.count(t, "SELECT count(*) FROM organizations WHERE kind = 'university'"); n != orgsBefore || n != len(data.Universities) {
		t.Fatalf("university orgs %d, before %d", n, orgsBefore)
	}
	// The .edu.tr resolution works on the imported domains.
	if _, found, err := e.svc.UniversityByEmailDomain(ctx, "ogr.itu.edu.tr"); err != nil || !found {
		t.Fatalf("ogr.itu.edu.tr: %v, %v", found, err)
	}
}

// TestAdminCreateClub_CreatesOrgInSameTx_Gorm: the club's org is written in
// directory's transaction (orgs.OrgAdmin.Create joins it, 01 §6.3): when
// the clubs row fails, the org is rolled back too.
func TestAdminCreateClub_CreatesOrgInSameTx_Gorm(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t)
	ctx := t.Context()
	if _, err := e.svc.ImportReference(ctx, directory.ReferenceData{
		Universities: []directory.RefUniversity{{YokCode: "PG-1", Name: "Pg Üniversitesi", City: "İzmir", Type: directory.UniversityState}},
	}); err != nil {
		t.Fatal(err)
	}
	u, err := e.store.UniversityByYokCode(ctx, "PG-1")
	if err != nil {
		t.Fatal(err)
	}
	actor := authz.Actor{Kind: authz.KindUser, UserID: pgUser(e.d)(t), MFAVerified: true, PlatformRoles: []authz.PlatformRole{authz.PlatformAdmin}}
	// The org and a clubs row that fails its FK in one transaction (the
	// service's order: OrgAdmin.Create, then InsertClub): nothing remains.
	clubsBefore := e.count(t, "SELECT count(*) FROM organizations WHERE kind = 'club'")
	boom := errors.New("caller failed after the org")
	err = e.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		id, err := e.orgs.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Geri Alınan Kulüp"})
		if err != nil {
			return err
		}
		if _, err := e.store.InsertClub(ctx, directory.Club{OrgID: id, UniversityID: uuid.NewV7(), Category: directory.CategoryArts,
			JoinPolicy: directory.JoinOpen, ClaimStatus: directory.ClaimUnclaimed}); err == nil {
			return errors.New("club of an unknown university was written")
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("transaction = %v", err)
	}
	if n := e.count(t, "SELECT count(*) FROM organizations WHERE kind = 'club'"); n != clubsBefore {
		t.Fatalf("club orgs %d after the rollback, %d before", n, clubsBefore)
	}
	// Through the service: a sensitive category without the flag fails
	// before any write; a valid club writes the org and the row together.
	_, err = e.svc.AdminCreateClub(ctx, actor, directory.ClubInput{UniversityID: u.ID, Name: "İnanç Kulübü",
		Category: directory.CategoryReligious, JoinPolicy: directory.JoinApproval})
	wantCode(t, err, 422, "directory.sensitivity_required")
	v, err := e.svc.AdminCreateClub(ctx, actor, directory.ClubInput{UniversityID: u.ID, Name: "Kalıcı Kulüp",
		Category: directory.CategoryArts, JoinPolicy: directory.JoinApproval})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, "SELECT count(*) FROM clubs c JOIN organizations o ON o.id = c.org_id AND o.kind = 'club' WHERE c.org_id = $1",
		v.Club.OrgID); n != 1 {
		t.Fatalf("club row and org: %d", n)
	}
	if n := e.count(t, "SELECT count(*) FROM organizations WHERE kind = 'club'"); n != clubsBefore+1 {
		t.Fatalf("club orgs %d, want %d", n, clubsBefore+1)
	}
}

// TestSetSensitivity_EmitsEventInSameTx_Gorm: the flag and the outbox row
// of club.sensitivity_changed commit together.
func TestSetSensitivity_EmitsEventInSameTx_Gorm(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t)
	ctx := t.Context()
	if _, err := e.svc.ImportReference(ctx, directory.ReferenceData{
		Universities: []directory.RefUniversity{{YokCode: "PG-2", Name: "Pg İki Üniversitesi", City: "Ankara", Type: directory.UniversityState}},
	}); err != nil {
		t.Fatal(err)
	}
	u, _ := e.store.UniversityByYokCode(ctx, "PG-2")
	actor := authz.Actor{Kind: authz.KindUser, UserID: pgUser(e.d)(t), MFAVerified: true, PlatformRoles: []authz.PlatformRole{authz.PlatformAdmin}}
	v, err := e.svc.AdminCreateClub(ctx, actor, directory.ClubInput{UniversityID: u.ID, Name: "Felsefe Kulübü",
		Category: directory.CategoryCulture, JoinPolicy: directory.JoinApproval})
	if err != nil {
		t.Fatal(err)
	}
	outboxRows := func() int {
		return e.count(t, "SELECT count(*) FROM outbox WHERE topic = 'club.sensitivity_changed' AND aggregate_id = $1", v.Club.OrgID)
	}
	proof := identity.ReauthProof{Password: goodPassword}
	in := directory.SensitivityInput{IsSensitive: true, SensitiveReason: "philosophical_belief"}
	// A failing audit write rolls back the flag and the outbox row.
	e.audit.setFail(errors.New("audit down"))
	if _, err := e.svc.AdminSetClubSensitivity(ctx, actor, v.Club.OrgID, in, proof); err == nil {
		t.Fatal("succeeded with a failing audit")
	}
	e.audit.setFail(nil)
	if c, _ := e.store.GetClub(ctx, v.Club.OrgID); c.IsSensitive || outboxRows() != 0 {
		t.Fatalf("after the rollback: sensitive=%v outbox=%d", c.IsSensitive, outboxRows())
	}
	got, err := e.svc.AdminSetClubSensitivity(ctx, actor, v.Club.OrgID, in, proof)
	if err != nil || !got.Club.IsSensitive || got.Club.SensitivitySetAt == nil {
		t.Fatalf("set = %+v, %v", got.Club, err)
	}
	if n := outboxRows(); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
	// The religious category is allowed now; clearing the flag of a
	// religious club is refused by the service and by the CHECK.
	if _, err := e.d.Pool.Exec(ctx, `UPDATE clubs SET category = 'religious' WHERE org_id = $1`, v.Club.OrgID); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AdminSetClubSensitivity(ctx, actor, v.Club.OrgID, directory.SensitivityInput{}, proof)
	wantCode(t, err, 422, "directory.sensitivity_required")
	if _, err := e.d.Pool.Exec(ctx, `UPDATE clubs SET is_sensitive = false, sensitive_reason = NULL, sensitivity_set_at = NULL
		WHERE org_id = $1`, v.Club.OrgID); err == nil {
		t.Fatal("the CHECK let a religious club lose its flag")
	}
}

// TestListClubs_TurkishSearch_IstanbulVariants_Gorm runs the Turkish search
// against PostgreSQL (koro_tr FTS, koro_norm = tr-TR lower + unaccent,
// pg_trgm) through the service.
func TestListClubs_TurkishSearch_IstanbulVariants_Gorm(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t)
	ctx := t.Context()
	if _, err := e.svc.ImportReference(ctx, directory.ReferenceData{
		Universities: []directory.RefUniversity{{YokCode: "PG-3", Name: "Pg Üç Üniversitesi", City: "İzmir", Type: directory.UniversityState}},
	}); err != nil {
		t.Fatal(err)
	}
	u, _ := e.store.UniversityByYokCode(ctx, "PG-3")
	actor := authz.Actor{Kind: authz.KindUser, UserID: pgUser(e.d)(t), MFAVerified: true, PlatformRoles: []authz.PlatformRole{authz.PlatformAdmin}}
	for _, n := range []string{"İstanbul Gezginleri", "ISTANBUL Tiyatro Topluluğu", "Işık Fotoğrafçılık", "Satranç Kulübü"} {
		if _, err := e.svc.AdminCreateClub(ctx, actor, directory.ClubInput{UniversityID: u.ID, Name: n,
			Category: directory.CategoryCulture, JoinPolicy: directory.JoinApproval}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"ISTANBUL Tiyatro Topluluğu", "İstanbul Gezginleri"}
	for _, q := range []string{"istanbul", "İstanbul", "İSTANBUL", "ISTANBUL", "Istanbul", "ıstanbul", "Istanbl"} {
		page, err := e.svc.ListClubs(ctx, authz.Anonymous(), directory.ClubQuery{Q: q})
		if err != nil {
			t.Fatal(err)
		}
		if got := names(page); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("q=%q: %v, want %v", q, got, want)
		}
	}
	for q, w := range map[string]string{"ışık": "Işık Fotoğrafçılık", "ISIK": "Işık Fotoğrafçılık", "satranc": "Satranç Kulübü",
		"tiyatrolar": "ISTANBUL Tiyatro Topluluğu"} { // plural: Turkish stemming (FTS) or word similarity
		page, err := e.svc.ListClubs(ctx, authz.Anonymous(), directory.ClubQuery{Q: q})
		if err != nil {
			t.Fatal(err)
		}
		if got := names(page); len(got) != 1 || got[0] != w {
			t.Errorf("q=%q: %v, want [%s]", q, got, w)
		}
	}
	// Turkish name order (tr collation): I < İ.
	page, err := e.svc.ListClubs(ctx, authz.Anonymous(), directory.ClubQuery{Sort: directory.SortName})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range page.Items {
		order = append(order, c.Row.Name)
	}
	wantOrder := []string{"ISTANBUL Tiyatro Topluluğu", "Işık Fotoğrafçılık", "İstanbul Gezginleri", "Satranç Kulübü"}
	if len(order) != 4 || order[0] != wantOrder[0] || order[1] != wantOrder[1] || order[2] != wantOrder[2] || order[3] != wantOrder[3] {
		t.Fatalf("name order = %v, want %v", order, wantOrder)
	}
}
