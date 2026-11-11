//go:build integration

package ingest_test

import (
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/ingest"
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

// pgModules are orgs and directory over PostgreSQL (the parents of the
// ingest rows and the club import of the pipeline).
type pgModules struct {
	d   *db.DB
	dir directory.Service
	org orgs.Service
}

func newPGModules(t *testing.T) pgModules {
	t.Helper()
	d := testutil.NewDB(t)
	clk := clock.NewFake(t0)
	log := obs.Discard()
	out := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: outbox.NewRegistry(), Queue: queue.NewMem(), Clock: clk, Log: log})
	idem := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: log})
	al := &audittest.Recorder{}
	om, err := orgs.New(orgs.Deps{Store: orgs.NewGormStore(d.ORM), Tx: d.Tx, Outbox: out, Audit: al, Idempotency: idem, Clock: clk, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	dm, err := directory.New(directory.Deps{Store: directory.NewGormStore(d.ORM), Tx: d.Tx, Outbox: out, Queue: queue.NewMem(),
		Audit: al, Orgs: om.Service, Idempotency: idem, Clock: clk, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	return pgModules{d: d, dir: dm.Service, org: om.Service}
}

func (p pgModules) world() world {
	return world{
		uni: func(t *testing.T) uuid.UUID {
			t.Helper()
			code := "ING" + next()
			if _, err := p.dir.ImportReference(t.Context(), directory.ReferenceData{Universities: []directory.RefUniversity{
				{YokCode: code, Name: "Üniversite " + code, City: "Ankara", Type: directory.UniversityState}}}); err != nil {
				t.Fatal(err)
			}
			return universityID(t, p.d, code)
		},
		club: func(t *testing.T, uni uuid.UUID) uuid.UUID {
			t.Helper()
			id, _, err := p.dir.ImportClub(t.Context(), directory.ImportedClub{UniversityID: uni, Name: "Kulüp " + next(),
				Category: directory.CategoryHobby, Job: "test"})
			if err != nil {
				t.Fatal(err)
			}
			return id
		},
	}
}

func universityID(t *testing.T, d *db.DB, code string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := d.Pool.QueryRow(t.Context(), `SELECT id FROM universities WHERE yok_code = $1`, code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (ingest.Store, world) {
		p := newPGModules(t)
		return ingest.NewGormStore(p.d.ORM), p.world()
	})
}

// TestPipeline_ClubListAndEvents_Gorm (BE-210): the pipeline over
// PostgreSQL: the official list opens the clubs through directory in the
// ingest transaction (org, clubs row, imported_clubs row, the club's site
// source), the same list again opens nothing, and the events page stores
// its event once.
func TestPipeline_ClubListAndEvents_Gorm(t *testing.T) {
	t.Parallel()
	p := newPGModules(t)
	ctx := t.Context()
	uni := p.world().uni(t)
	clk := clock.NewFake(t0)
	fetch := ingest.NewMemFetcher()
	al := &audittest.Recorder{}
	mod, err := ingest.New(ingest.Deps{
		Store: ingest.NewGormStore(p.d.ORM), Tx: p.d.Tx, Directory: p.dir, Orgs: p.org, Fetcher: fetch,
		Extractor: ingest.FakeExtractor{}, Audit: al, Clock: clk, Log: obs.Discard(),
		Config: ingest.Config{FetchEnabled: true, SourceDefaultEnabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	pad := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), SessionID: uuid.NewV7(), MFAVerified: true,
		PlatformRoles: []authz.PlatformRole{authz.PlatformAdmin}}
	list, err := mod.Service.AdminCreateSource(ctx, pad, ingest.SourceInput{Kind: ingest.KindClubList,
		Adapter: ingest.AdapterClubListBOUN, URL: bounList, Label: "Liste", UniversityID: uni})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("testdata/boun_clubs.html")
	if err != nil {
		t.Fatal(err)
	}
	fetch.Set(bounList, ingest.Page{ContentType: "text/html", Body: b})
	tick := func() {
		t.Helper()
		clk.Advance(time.Minute)
		if _, err := mod.Service.RunDue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := p.d.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM clubs WHERE university_id = $1 AND claim_status = 'unclaimed'`, uni); n != 5 {
		t.Fatalf("clubs = %d", n)
	}
	if n := count(`SELECT count(*) FROM imported_clubs WHERE university_id = $1`, uni); n != 5 {
		t.Fatalf("imported clubs = %d", n)
	}
	if n := count(`SELECT count(*) FROM ingest_sources WHERE club_org_id IS NOT NULL AND university_id = $1`, uni); n != 1 {
		t.Fatalf("club site sources = %d", n)
	}
	if _, err := mod.Service.AdminRunSource(ctx, pad, list.Source.ID); err != nil {
		t.Fatal(err)
	}
	fetch.Set(bounList, ingest.Page{ContentType: "text/html", Body: append(b, []byte("<!-- changed -->")...)})
	tick()
	if n := count(`SELECT count(*) FROM clubs WHERE university_id = $1`, uni); n != 5 {
		t.Fatalf("clubs after a second list = %d", n)
	}

	page := "https://kim.metu.edu.tr/tr/etkinlikler"
	if _, err := mod.Service.AdminCreateSource(ctx, pad, ingest.SourceInput{Kind: ingest.KindEvents,
		Adapter: ingest.AdapterHTMLPage, URL: page, Label: "Etkinlikler", UniversityID: uni}); err != nil {
		t.Fatal(err)
	}
	fetch.Set(page, ingest.Page{ContentType: "text/html", Body: []byte(
		"<main><p>Satranç Turnuvası 21.11.2026 saat 14:00</p><p>Satranç Turnuvası 21.11.2026 saat 14:00</p></main>")})
	tick()
	tick()
	if n := count(`SELECT count(*) FROM imported_events WHERE university_id = $1`, uni); n != 1 {
		t.Fatalf("imported events = %d", n)
	}
	l, err := mod.Service.ListImportedEvents(ctx, authz.Anonymous(), ingest.EventQuery{UniversityID: &uni})
	if err != nil || len(l.Items) != 1 || !strings.HasPrefix(l.Items[0].Event.Title, "Satranç Turnuvası") {
		t.Fatalf("public list = %+v %v", l, err)
	}
}
