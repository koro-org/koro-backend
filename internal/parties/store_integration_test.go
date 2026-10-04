//go:build integration

package parties_test

import (
	"bytes"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var seq atomic.Int64

// pgWorld inserts the parent rows (organizations, users, files, payees) as app_rw.
func pgWorld(d *db.DB) world {
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	user := func(t *testing.T) uuid.UUID {
		id := uuid.NewV7()
		exec(t, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
			VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, "u"+strconv.FormatInt(seq.Add(1), 10)+"@example.test")
		return id
	}
	return world{
		org: func(t *testing.T) uuid.UUID {
			id, n := uuid.NewV7(), strconv.FormatInt(seq.Add(1), 10)
			exec(t, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'company', $2, $3)`, id, "Şirket "+n, "sirket-"+n)
			return id
		},
		user: user,
		file: func(t *testing.T) uuid.UUID {
			id := uuid.NewV7()
			exec(t, `INSERT INTO files (id, purpose, bucket, storage_key, declared_mime, size_bytes, sha256, status, scan_status)
				VALUES ($1, 'party_document', 'koro-files', $2, 'application/pdf', 10, $3, 'active', 'clean')`,
				id, "party_document/"+id.String(), bytes.Repeat([]byte{1}, 32))
			return id
		},
		payee: func(t *testing.T, org, party uuid.UUID, status string) {
			by := user(t)
			var verifiedAt *time.Time
			var verifiedBy *uuid.UUID
			if status == "active" {
				now := time.Now()
				verifiedAt, verifiedBy = &now, &by
			}
			exec(t, `INSERT INTO payees (id, legal_party_id, org_id, status, verified_at, verified_by, created_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, uuid.NewV7(), party, org, status, verifiedAt, verifiedBy, by)
		},
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (parties.Store, world) {
		d := testutil.NewDB(t)
		return parties.NewGormStore(d.ORM), pgWorld(d)
	})
}

// TestService_Gorm_EndToEnd runs the module over PostgreSQL: a buyer party
// with its address (jsonb), a document attached through a real files row,
// the ETag of a real updated_at, verification and the reset on change,
// the seed and the individual path (sealed national ID, blind index).
func TestService_Gorm_EndToEnd(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	store := parties.NewGormStore(d.ORM)
	au := audit.New(audit.Deps{Store: audit.NewGormStore(d.ORM), Tx: d.Tx, Clock: clk, Log: discard()}).Service
	fs := newFakeFiles()
	o := newFakeOrgs()
	company := w.org(t)
	o.m[company] = o.m[org("company_a")]
	sealer, indexer := testKeys(t)
	mod, err := parties.New(parties.Deps{
		Store: store, Tx: d.Tx, Audit: au, Orgs: fakeOrgsFor(o, company), Files: fs, Clock: clk, Log: discard(),
		Sealer: sealer, Indexer: indexer, AllowNaturalPersonPayees: true,
		Idempotency: idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: discard()}),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := mod.Service
	// The personas act as users that exist in this database.
	cma := authz.Actor{Kind: authz.KindUser, UserID: w.user(t), MFAVerified: true,
		Memberships: []authz.Membership{{OrgID: company, OrgKind: authz.OrgCompany, Role: authz.MemberCompanyAdmin}}}
	fin := authz.Actor{Kind: authz.KindUser, UserID: w.user(t), MFAVerified: true, PlatformRoles: []authz.PlatformRole{authz.PlatformFinanceAdmin}}

	v, err := svc.CreateBuyerParty(ctxOf(t, cma), cma, company, buyerInput(vknA))
	if err != nil {
		t.Fatal(err)
	}
	if v.Party.Address.City != "İstanbul" || v.Party.Address.Country != "TR" || v.Party.Status != parties.StatusPendingVerification {
		t.Fatalf("created = %+v", v.Party)
	}
	fileID := w.file(t)
	fs.meta[fileID] = files.FileMeta{ID: fileID, Purpose: files.PurposePartyDocument, UploadedBy: &cma.UserID, OrgID: &company,
		Status: files.StatusActive, ScanStatus: files.ScanClean}
	doc, err := svc.UploadDocument(ctxOf(t, cma), cma, company, v.Party.ID, parties.DocumentInput{Kind: parties.DocTaxCertificate, FileID: fileID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdminVerifyDocument(ctxOf(t, fin), fin, doc.ID); err != nil {
		t.Fatal(err)
	}
	ver, err := svc.AdminVerifyParty(ctxOf(t, fin), fin, v.Party.ID, parties.Decision{Decision: parties.StatusVerified})
	if err != nil || ver.Party.Status != parties.StatusVerified || len(ver.Documents) != 1 || ver.Documents[0].VerifiedAt == nil {
		t.Fatalf("verified = %+v %v", ver, err)
	}
	if p, found, err := svc.BuyerPartyOf(t.Context(), company); err != nil || !found || p.ID != v.Party.ID {
		t.Fatalf("BuyerPartyOf = %v %v %v", p.ID, found, err)
	}
	_, err = svc.UpdateOrgParty(ctxOf(t, cma), cma, company, v.Party.ID, v.ETag, parties.PartyPatch{Title: ptr("Yeni Ünvan A.Ş.")})
	wantErr(t, err, parties.ErrPreconditionFailed)
	upd, err := svc.UpdateOrgParty(ctxOf(t, cma), cma, company, v.Party.ID, ver.ETag, parties.PartyPatch{Title: ptr("Yeni Ünvan A.Ş.")})
	if err != nil || upd.Party.Status != parties.StatusPendingVerification || upd.Party.VerifiedAt != nil || upd.ETag == ver.ETag {
		t.Fatalf("update of a verified party: %+v %v", upd.Party, err)
	}
	got, err := svc.GetOrgParty(ctxOf(t, cma), cma, company, v.Party.ID)
	if err != nil || got.ETag != upd.ETag || got.Party.Title != "Yeni Ünvan A.Ş." {
		t.Fatalf("read back: %+v %v", got, err)
	}

	// Seed: once, then unchanged.
	sd, err := parties.NewSeeder(parties.SeederDeps{Store: store, Tx: d.Tx, Audit: au, Clock: clk, Log: discard()})
	if err != nil {
		t.Fatal(err)
	}
	p1, c1, err := sd.EnsurePlatform(t.Context())
	if err != nil || !c1 || p1.Status != parties.StatusDraft {
		t.Fatalf("seed: %+v %v %v", p1, c1, err)
	}
	p2, c2, err := sd.EnsurePlatform(t.Context())
	if err != nil || c2 || p2.ID != p1.ID {
		t.Fatalf("second seed: %+v %v %v", p2, c2, err)
	}

	// Individuals: sealed and indexed in PostgreSQL; a duplicate is 409.
	ind := parties.PartyInput{Kind: parties.KindPayee, LegalType: parties.LegalIndividual, Title: "Ali Veli", TCKN: tcknA,
		Address: addr()}
	iv, err := svc.AdminCreateParty(ctxOf(t, fin), fin, ind)
	if err != nil || !iv.TCKNOnFile {
		t.Fatalf("individual: %+v %v", iv, err)
	}
	var enc, bidx []byte
	if err := d.Pool.QueryRow(t.Context(), `SELECT tckn_enc, tckn_bidx FROM legal_parties WHERE id = $1`, iv.Party.ID).Scan(&enc, &bidx); err != nil {
		t.Fatal(err)
	}
	if len(enc) == 0 || bytes.Contains(enc, []byte(tcknA)) || len(bidx) != 32 {
		t.Fatalf("stored national ID: %d / %d bytes", len(enc), len(bidx))
	}
	_, err = svc.AdminCreateParty(ctxOf(t, fin), fin, ind)
	wantProblem(t, err, 409, "parties.tax_id_taken")
	// The audit rows exist (one per write and the document verification).
	var n int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action LIKE 'parties.%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 7 {
		t.Fatalf("%d parties audit rows", n)
	}
}

// fakeOrgsFor returns o (company is registered as a company org).
func fakeOrgsFor(o *fakeOrgs, company uuid.UUID) *fakeOrgs {
	s := o.m[company]
	s.ID = company
	o.m[company] = s
	return o
}
