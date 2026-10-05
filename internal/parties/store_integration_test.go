//go:build integration

package parties_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
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
		Sealer: sealer, Indexer: indexer, AllowNaturalPersonPayees: true, Outbox: outbox.NewMem(clk), Queue: queue.NewMem(),
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

func TestPayeeStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	payeeStoreContract(t, func(t *testing.T) (parties.Store, world) {
		d := testutil.NewDB(t)
		return parties.NewGormStore(d.ORM), pgWorld(d)
	})
}

// failingAudit fails the audit record of one action (the business write of
// that action must roll back with it).
type failingAudit struct {
	audit.Logger
	act authz.Action
}

func (f failingAudit) Log(ctx context.Context, e audit.Event) error {
	if e.Action == string(f.act) {
		return errors.New("audit store down")
	}
	return f.Logger.Log(ctx, e)
}

// TestBankAccount_Gorm_ApproveRollsBackTogether runs the bank account flow
// over PostgreSQL: the four eyes (authz and the CHECKs answered 403), the
// approval that supersedes the approved account — rolled back together when
// the approval's audit record fails (one transaction) — the frozen IBAN and
// the outbox rows.
func TestBankAccount_Gorm_ApproveRollsBackTogether(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	store := parties.NewGormStore(d.ORM)
	base := audit.New(audit.Deps{Store: audit.NewGormStore(d.ORM), Tx: d.Tx, Clock: clk, Log: discard()}).Service
	fs := newFakeFiles()
	o := newFakeOrgs()
	club := w.org(t)
	o.m[club] = orgs.OrgSummary{ID: club, Kind: orgs.KindClub, Name: "Kulüp", Status: orgs.StatusActive, Slug: "kulup"}
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: outbox.NewRegistry(), Queue: queue.NewMem(),
		Clock: clk, Log: discard()})
	build := func(au parties.AuditLogger) parties.Service {
		mod, err := parties.New(parties.Deps{
			Store: store, Tx: d.Tx, Audit: au, Orgs: o, Files: fs, Clock: clk, Log: discard(), Outbox: ob, Queue: queue.NewMem(),
			Reauth:      &fakeReauth{},
			Idempotency: idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: discard()}),
		})
		if err != nil {
			t.Fatal(err)
		}
		return mod.Service
	}
	svc := build(base)
	staff := func(r authz.PlatformRole) authz.Actor {
		return authz.Actor{Kind: authz.KindUser, UserID: w.user(t), MFAVerified: true, PlatformRoles: []authz.PlatformRole{r}}
	}
	fin, fin2, pad := staff(authz.PlatformFinanceAdmin), staff(authz.PlatformFinanceAdmin), staff(authz.PlatformAdmin)
	cla := authz.Actor{Kind: authz.KindUser, UserID: w.user(t),
		Memberships: []authz.Membership{{OrgID: club, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}}

	party, err := svc.AdminCreateParty(ctxOf(t, fin), fin, foundationInput(vknC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdminVerifyParty(ctxOf(t, fin2), fin2, party.Party.ID, parties.Decision{Decision: parties.StatusVerified}); err != nil {
		t.Fatal(err)
	}
	py, err := svc.AdminCreatePayee(ctxOf(t, fin), fin, parties.PayeeInput{LegalPartyID: party.Party.ID, OrgID: club, IsDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdminVerifyPayee(ctxOf(t, pad), pad, py.Payee.ID); err != nil {
		t.Fatal(err)
	}
	evidence := func(by authz.Actor) uuid.UUID {
		id := w.file(t)
		fs.meta[id] = files.FileMeta{ID: id, Purpose: files.PurposeBankAccountEvidence, UploadedBy: &by.UserID, OrgID: &club,
			Status: files.StatusActive, ScanStatus: files.ScanClean}
		return id
	}
	submit := func(number string) parties.BankAccountView {
		t.Helper()
		v, err := svc.SubmitPayeeBankAccount(ctxOf(t, cla), cla, club, py.Payee.ID, parties.BankAccountInput{IBAN: number,
			HolderName: "Örnek Eğitim Vakfı", EvidenceFileID: evidence(cla)}, proof())
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		return v
	}
	old := submit(ibanA)
	if _, err := svc.AdminVerifyBankAccount(ctxOf(t, fin), fin, old.Account.ID, parties.VerifyInput{HolderMatchesParty: true, EvidenceOK: true}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.AdminApproveBankAccount(ctxOf(t, fin), fin, old.Account.ID, proof())
	wantProblem(t, err, 403, "authz.four_eyes_violation")
	if _, err := svc.AdminApproveBankAccount(ctxOf(t, pad), pad, old.Account.ID, proof()); err != nil {
		t.Fatal(err)
	}

	// The database refuses the eyes on its own (03 §6.1 second layer), answered 403.
	for _, sql := range []string{
		`UPDATE payee_bank_accounts SET approved_by = submitted_by WHERE id = $1`,
		`UPDATE payee_bank_accounts SET approved_by = verified_by WHERE id = $1`,
	} {
		err := d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
			return db.Conn(ctx, d.ORM).Exec(sql, old.Account.ID).Error
		})
		if db.ConstraintName(err) == "" {
			t.Fatalf("%s: %v, want a four-eyes CHECK", sql, err)
		}
	}
	// The IBAN of an account never changes (payee_bank_accounts_frozen).
	if _, err := d.Pool.Exec(t.Context(), `UPDATE payee_bank_accounts SET iban = $2 WHERE id = $1`, old.Account.ID, ibanB); err == nil ||
		db.SQLState(err) != db.CodeRestrictViolation {
		t.Fatalf("IBAN change: %v, want 23001", err)
	}

	// The change: verified, then the approval fails at its audit record —
	// the supersede and the approval roll back together.
	next := submit(ibanB)
	if _, err := svc.AdminVerifyBankAccount(ctxOf(t, fin2), fin2, next.Account.ID, parties.VerifyInput{HolderMatchesParty: true, EvidenceOK: true}); err != nil {
		t.Fatal(err)
	}
	broken := build(failingAudit{Logger: base, act: authz.PartiesBankAccountApprove})
	if _, err := broken.AdminApproveBankAccount(ctxOf(t, pad), pad, next.Account.ID, proof()); err == nil {
		t.Fatal("the approval succeeded without its audit record")
	}
	statusOf := func(id uuid.UUID) string {
		var s string
		if err := d.Pool.QueryRow(t.Context(), `SELECT status FROM payee_bank_accounts WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if statusOf(old.Account.ID) != "approved" || statusOf(next.Account.ID) != "pending_approval" {
		t.Fatalf("after the failed approval: old %s, new %s", statusOf(old.Account.ID), statusOf(next.Account.ID))
	}
	if _, err := svc.AdminApproveBankAccount(ctxOf(t, pad), pad, next.Account.ID, proof()); err != nil {
		t.Fatal(err)
	}
	if statusOf(old.Account.ID) != "superseded" || statusOf(next.Account.ID) != "approved" {
		t.Fatalf("after the approval: old %s, new %s", statusOf(old.Account.ID), statusOf(next.Account.ID))
	}
	var superseder uuid.UUID
	if err := d.Pool.QueryRow(t.Context(), `SELECT superseded_by_id FROM payee_bank_accounts WHERE id = $1`, old.Account.ID).Scan(&superseder); err != nil || superseder != next.Account.ID {
		t.Fatalf("superseded_by_id = %s, %v", superseder, err)
	}
	var events int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox WHERE topic LIKE 'payee_bank_account.%'`).Scan(&events); err != nil || events != 6 {
		t.Fatalf("outbox rows = %d, %v (2 × submitted at submission and verification, 2 × approved)", events, err)
	}
}
