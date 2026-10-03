//go:build integration

package migrations_test

import (
	"sync"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/iban"
	"koro/internal/platform/iban/ibantest"
)

// try runs one statement in a transaction of the app_rw pool, rolls it back
// and returns its SQLSTATE ("" on success) and constraint name.
func (w *p1aWorld) try(sql string, args ...any) (state, constraint string) {
	w.t.Helper()
	tx, err := w.d.Pool.Begin(w.t.Context())
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(w.t.Context()) }()
	_, err = tx.Exec(w.t.Context(), sql, args...)
	return db.SQLState(err), db.ConstraintName(err)
}

// TestSchema_LegalPartyConstraints (02 §17.1): the national ID is stored only
// encrypted with its blind index and only for an individual, the verified
// state carries both verification columns (06 H-37), Koro has one platform
// party, a buyer party belongs to an org, tax numbers are unique per kind
// among live rows.
func TestSchema_LegalPartyConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	admin, company := w.user(), w.org("company")
	ins := `INSERT INTO legal_parties (kind, legal_type, org_id, title, tax_no, tckn_enc, tckn_bidx, status, verified_at, verified_by, address)
		VALUES ($1, $2, $3, 'Örnek Vakfı', $4, $5, $6, $7, $8, $9, $10)`
	enc, bidx := []byte("envelope"), bytes32("tckn")
	now := "2026-10-03T00:00:00Z"
	expectFail(t, w.d.Pool, []p1aCase{
		{"an individual without the encrypted TCKN", ins, []any{"payee", "individual", nil, nil, nil, bidx, "draft", nil, nil, "{}"}, chk, "legal_parties_tckn_chk"},
		{"an individual without the blind index", ins, []any{"payee", "individual", nil, nil, enc, nil, "draft", nil, nil, "{}"}, chk, "legal_parties_tckn_chk"},
		{"partial: a foundation with a blind index only", ins, []any{"payee", "foundation", nil, "1234567890", nil, bidx, "draft", nil, nil, "{}"}, chk, "legal_parties_tckn_chk"},
		{"partial: a foundation with an encrypted TCKN only", ins, []any{"payee", "foundation", nil, "1234567890", enc, nil, "draft", nil, nil, "{}"}, chk, "legal_parties_tckn_chk"},
		{"a blind index of the wrong size", ins, []any{"payee", "individual", nil, nil, enc, []byte("short"), "draft", nil, nil, "{}"}, chk, "legal_parties_tckn_bidx_chk"},
		{"verified without the verifier", ins, []any{"payee", "foundation", nil, "1234567890", nil, nil, "verified", now, nil, "{}"}, chk, "legal_parties_verified_chk"},
		{"verified without the time", ins, []any{"payee", "foundation", nil, "1234567890", nil, nil, "verified", nil, admin, "{}"}, chk, "legal_parties_verified_chk"},
		{"partial: a draft with a verifier only", ins, []any{"payee", "foundation", nil, "1234567890", nil, nil, "draft", nil, admin, "{}"}, chk, "legal_parties_verified_chk"},
		{"partial: a rejected party with a verification time only", ins, []any{"payee", "foundation", nil, "1234567890", nil, nil, "rejected", now, nil, "{}"}, chk, "legal_parties_verified_chk"},
		{"a verified company without a tax number", ins, []any{"payee", "foundation", nil, nil, nil, nil, "verified", now, admin, "{}"}, chk, "legal_parties_verified_tax_no_chk"},
		{"a buyer party without an org", ins, []any{"buyer_company", "company", nil, "1234567890", nil, nil, "draft", nil, nil, "{}"}, chk, "legal_parties_buyer_org_chk"},
		{"a tax number of nine digits", ins, []any{"payee", "foundation", nil, "123456789", nil, nil, "draft", nil, nil, "{}"}, chk, "legal_parties_tax_no_chk"},
		{"an address that is not an object", ins, []any{"payee", "foundation", nil, "1234567890", nil, nil, "draft", nil, nil, "[]"}, chk, "legal_parties_address_chk"},
		{"an unknown kind", ins, []any{"seller", "foundation", nil, "1234567890", nil, nil, "draft", nil, nil, "{}"}, chk, "legal_parties_kind_chk"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"an individual payee", ins, []any{"payee", "individual", nil, nil, enc, bidx, "verified", now, admin, "{}"}, "", ""},
		{"a verified foundation", ins, []any{"payee", "foundation", nil, "1234567890", nil, nil, "verified", now, admin, `{"city":"Ankara"}`}, "", ""},
		{"a buyer company", ins, []any{"buyer_company", "company", company, "1234567891", nil, nil, "draft", nil, nil, "{}"}, "", ""},
		{"the same tax number as a buyer (another kind)", ins, []any{"buyer_company", "company", company, "1234567890", nil, nil, "draft", nil, nil, "{}"}, "", ""},
		{"an inactive duplicate", ins, []any{"payee", "foundation", nil, "1234567890", nil, nil, "inactive", nil, nil, "{}"}, "", ""},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{"a second live payee with the tax number", ins, []any{"payee", "foundation", nil, "1234567890", nil, nil, "draft", nil, nil, "{}"}, db.CodeUniqueViolation, "legal_parties_taxno_uq"},
		{"a second individual with the blind index", ins, []any{"payee", "individual", nil, nil, enc, bidx, "draft", nil, nil, "{}"}, db.CodeUniqueViolation, "legal_parties_tckn_uq"},
	})
	w.platformParty()
	platform := w.platformOrg()
	expectFail(t, w.d.Pool, []p1aCase{
		{"a second platform party", ins, []any{"platform", "company", platform, "9999999999", nil, nil, "draft", nil, nil, "{}"}, db.CodeUniqueViolation, "legal_parties_platform_uq"},
		{"app_rw deletes a party", `DELETE FROM legal_parties`, nil, db.CodeInsufficientPriv, ""},
	})
}

// TestSchema_PayeeConstraints (02 §17.3): a payee is a legal party of kind
// 'payee' (composite FK on the kind), active only once verified, with
// verified_at / verified_by set together; an org has one active default
// payee and one open payee row per legal party.
func TestSchema_PayeeConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, creator, admin := w.org("club"), w.user(), w.user()
	party := w.legalParty("payee", "foundation", nil)
	company := w.org("company")
	buyerParty := w.legalParty("buyer_company", "company", &company)
	ins := `INSERT INTO payees (legal_party_id, org_id, status, is_default, verified_at, verified_by, created_by, payout_method, valid_to)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	now := "2026-10-03T00:00:00Z"
	expectFail(t, w.d.Pool, []p1aCase{
		{"a buyer party as payee", ins, []any{buyerParty, org, "draft", false, nil, nil, creator, "bank_eft", nil}, db.CodeForeignKeyViolation, "payees_legal_party_id_party_kind_fkey"},
		{"active without verification", ins, []any{party, org, "active", false, nil, nil, creator, "bank_eft", nil}, db.CodeCheckViolation, "payees_active_chk"},
		{"partial: a verifier without the time", ins, []any{party, org, "draft", false, nil, admin, creator, "bank_eft", nil}, db.CodeCheckViolation, "payees_verified_pair_chk"},
		{"partial: a verification time without the verifier", ins, []any{party, org, "active", false, now, nil, creator, "bank_eft", nil}, db.CodeCheckViolation, "payees_verified_pair_chk"},
		{"iyzico without a submerchant", ins, []any{party, org, "draft", false, nil, nil, creator, "iyzico", nil}, db.CodeCheckViolation, "payees_iyzico_chk"},
		{"valid_to before valid_from", ins, []any{party, org, "draft", false, nil, nil, creator, "bank_eft", "2000-01-01T00:00:00Z"}, db.CodeCheckViolation, "payees_validity_chk"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"the active default payee", ins, []any{party, org, "active", true, now, admin, creator, "bank_eft", nil}, "", ""},
		{"a closed earlier payee of the same party", ins, []any{party, org, "closed", false, nil, nil, creator, "bank_eft", nil}, "", ""},
	})
	other := w.legalParty("payee", "university", nil)
	expectFail(t, w.d.Pool, []p1aCase{
		{"a second open payee of the party", ins, []any{party, org, "draft", false, nil, nil, creator, "bank_eft", nil}, db.CodeUniqueViolation, "payees_org_party_uq"},
		{"a second active default", ins, []any{other, org, "active", true, now, admin, creator, "bank_eft", nil}, db.CodeUniqueViolation, "payees_default_uq"},
		{"app_rw deletes a payee", `DELETE FROM payees`, nil, db.CodeInsufficientPriv, ""},
	})
}

// TestSchema_LegalPartyDocumentConstraints (02 §17.2).
func TestSchema_LegalPartyDocumentConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	party, uploader, admin := w.legalParty("payee", "foundation", nil), w.user(), w.user()
	file := w.file("party_document")
	ins := `INSERT INTO legal_party_documents (legal_party_id, kind, file_id, uploaded_by, verified_at, verified_by) VALUES ($1, $2, $3, $4, $5, $6)`
	expectFail(t, w.d.Pool, []p1aCase{
		{"partial: a verifier without the time", ins, []any{party, "tax_certificate", file, uploader, nil, admin}, db.CodeCheckViolation, "legal_party_documents_verified_chk"},
		{"partial: a time without the verifier", ins, []any{party, "tax_certificate", file, uploader, "2026-10-03T00:00:00Z", nil}, db.CodeCheckViolation, "legal_party_documents_verified_chk"},
		{"an unknown kind", ins, []any{party, "selfie", file, uploader, nil, nil}, db.CodeCheckViolation, "legal_party_documents_kind_chk"},
	})
	w.exec(ins, party, "tax_certificate", file, uploader, nil, nil)
	expectFail(t, w.d.Pool, []p1aCase{
		{"one file, two documents", ins, []any{party, "other", file, uploader, nil, nil}, db.CodeUniqueViolation, "legal_party_documents_file_id_uq"},
		{"app_rw deletes a document", `DELETE FROM legal_party_documents`, nil, db.CodeInsufficientPriv, ""},
	})
}

// TestFourEyes_PayeeBankAccount_Checks (03 §6.1, 02 §17.4): the IBAN of a
// payee is approved by two different people; for an org submission three
// (submitter, verifier, approver). Every rule is a database CHECK or
// trigger, independent of the application: the submitter cannot verify or
// approve, the verifier cannot approve, an org submission is not approved
// without verification, an admin submission never waits for verification,
// a row is never born approved, one approved and one pending account per
// payee, and the IBAN of a row never changes.
func TestFourEyes_PayeeBankAccount_Checks(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	org := w.org("club")
	payee := w.payee(org, w.legalParty("payee", "foundation", nil))
	evidence := w.file("bank_account_evidence")
	clubAdmin, adminA, adminB := w.user(), w.user(), w.user()

	// Org submission: pending_verification → verify (A) → approve (B).
	org1, iban1 := uuid.NewV7(), w.iban()
	submit := `INSERT INTO payee_bank_accounts (id, payee_id, org_id, iban, holder_name, evidence_file_id, status, submitted_by, submitted_by_admin)
		VALUES ($1, $2, $3, $4, 'Ankara Üniversitesi Vakfı', $5, $6, $7, $8)`
	expectFail(t, w.d.Pool, []p1aCase{
		{"born approved", `INSERT INTO payee_bank_accounts (payee_id, org_id, iban, holder_name, evidence_file_id, status, submitted_by, submitted_by_admin, approved_by, approved_at)
			VALUES ($1, $2, $3, 'Vakıf', $4, 'approved', $5, true, $6, now())`, []any{payee, org, w.iban(), evidence, adminA, adminB}, chk, "payee_bank_accounts_initial_status_chk"},
		{"an admin submission waiting for verification", submit, []any{uuid.NewV7(), payee, org, w.iban(), evidence, "pending_verification", adminA, true}, chk, "payee_bank_accounts_admin_flow_chk"},
		{"an org submission straight to approval", submit, []any{uuid.NewV7(), payee, org, w.iban(), evidence, "pending_approval", clubAdmin, false}, chk, "payee_bank_accounts_org_flow_chk"},
		{"an invalid IBAN", submit, []any{uuid.NewV7(), payee, org, "TR330006100519786457841327", evidence, "pending_verification", clubAdmin, false}, chk, "payee_bank_accounts_iban_chk"},
		{"an account of another org", submit, []any{uuid.NewV7(), payee, w.org("club"), w.iban(), evidence, "pending_verification", clubAdmin, false}, db.CodeForeignKeyViolation, "payee_bank_accounts_payee_id_org_id_fkey"},
	})
	w.exec(submit, org1, payee, org, iban1, evidence, "pending_verification", clubAdmin, false)
	verify := `UPDATE payee_bank_accounts SET status = 'pending_approval', verified_by = $2, verified_at = now() WHERE id = $1`
	approve := `UPDATE payee_bank_accounts SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1`
	expectFail(t, w.d.Pool, []p1aCase{
		{"the submitter verifies", verify, []any{org1, clubAdmin}, chk, "payee_bank_accounts_verifier_submitter_chk"},
		{"approved without verification", approve, []any{org1, adminB}, chk, "payee_bank_accounts_org_flow_chk"},
		{"a second pending account", submit, []any{uuid.NewV7(), payee, org, w.iban(), evidence, "pending_verification", clubAdmin, false}, db.CodeUniqueViolation, "payee_bank_accounts_pending_uq"},
	})
	w.exec(verify, org1, adminA)
	expectFail(t, w.d.Pool, []p1aCase{
		{"the verifier approves", approve, []any{org1, adminA}, chk, "payee_bank_accounts_approver_verifier_chk"},
		{"the submitter approves", approve, []any{org1, clubAdmin}, chk, "payee_bank_accounts_approver_submitter_chk"},
		{"the IBAN changes after verification", `UPDATE payee_bank_accounts SET iban = $2 WHERE id = $1`, []any{org1, w.iban()}, db.CodeRestrictViolation, ""},
		{"the holder changes after verification", `UPDATE payee_bank_accounts SET holder_name = 'Başka Biri' WHERE id = $1`, []any{org1}, db.CodeRestrictViolation, ""},
		{"the submitter is rewritten", `UPDATE payee_bank_accounts SET submitted_by = $2 WHERE id = $1`, []any{org1, adminB}, db.CodeRestrictViolation, ""},
	})
	w.exec(approve, org1, adminB)

	// Admin submission: pending_approval (the submitter is the first eye) →
	// approve by another admin; the old account is superseded in the same
	// transaction (one approved account per payee).
	adm := uuid.NewV7()
	w.exec(submit, adm, payee, org, w.iban(), evidence, "pending_approval", adminA, true)
	expectFail(t, w.d.Pool, []p1aCase{
		{"the submitting admin approves", approve, []any{adm, adminA}, chk, "payee_bank_accounts_approver_submitter_chk"},
		{"two approved accounts", approve, []any{adm, adminB}, db.CodeUniqueViolation, "payee_bank_accounts_approved_uq"},
	})
	expectCommit(t, w, "superseded by an account of another payee", db.CodeForeignKeyViolation, "payee_bank_accounts_superseded_by_id_payee_id_fkey",
		st(`UPDATE payee_bank_accounts SET status = 'superseded', superseded_at = now(), superseded_by_id = $2 WHERE id = $1`,
			org1, w.seller().account))
	expectCommit(t, w, "superseded without the successor", chk, "payee_bank_accounts_superseded_chk",
		st(`UPDATE payee_bank_accounts SET status = 'superseded', superseded_at = now() WHERE id = $1`, org1))
	w.mustCommit(
		st(`UPDATE payee_bank_accounts SET status = 'superseded', superseded_at = now(), superseded_by_id = $2 WHERE id = $1`, org1, adm),
		st(approve, adm, adminB))
	if got := queryStrings(t, w, `SELECT status FROM payee_bank_accounts WHERE payee_id = $1 ORDER BY submitted_at, id`, payee); len(got) != 2 || got[0] != "superseded" || got[1] != "approved" {
		t.Errorf("accounts after the change = %v, want superseded then approved", got)
	}
	expectFail(t, w.d.Pool, []p1aCase{
		{"the approved IBAN changes", `UPDATE payee_bank_accounts SET iban = $2 WHERE id = $1`, []any{adm, w.iban()}, db.CodeRestrictViolation, ""},
		{"the payee changes", `UPDATE payee_bank_accounts SET payee_id = $2 WHERE id = $1`, []any{adm, w.payee(org, w.legalParty("payee", "foundation", nil))}, db.CodeRestrictViolation, ""},
		{"revoked without a reason", `UPDATE payee_bank_accounts SET status = 'revoked', revoked_by = $2, revoked_at = now() WHERE id = $1`, []any{adm, adminA}, chk, "payee_bank_accounts_revoked_chk"},
		{"app_rw deletes an account", `DELETE FROM payee_bank_accounts`, nil, db.CodeInsufficientPriv, ""},
	})
	w.exec(`UPDATE payee_bank_accounts SET status = 'revoked', revoked_by = $2, revoked_at = now(), revoke_reason = 'Hesap kapandı' WHERE id = $1`, adm, adminA)

	// A rejected org submission: rejected_by, rejected_at and the reason together.
	rej := uuid.NewV7()
	w.exec(submit, rej, payee, org, w.iban(), evidence, "pending_verification", clubAdmin, false)
	expectFail(t, w.d.Pool, []p1aCase{
		{"rejected without a reason", `UPDATE payee_bank_accounts SET status = 'rejected', rejected_by = $2, rejected_at = now() WHERE id = $1`, []any{rej, adminA}, chk, "payee_bank_accounts_rejected_chk"},
		{"approved straight from pending verification", approve, []any{rej, adminB}, chk, "payee_bank_accounts_org_flow_chk"},
	})
	w.exec(`UPDATE payee_bank_accounts SET status = 'rejected', rejected_by = $2, rejected_at = now(), rejection_reason = 'Unvan eşleşmiyor' WHERE id = $1`, rej, adminA)

	// Under concurrency only one submission of a payee can be pending.
	other := w.payee(org, w.legalParty("payee", "foundation", nil))
	var wg sync.WaitGroup
	ok := make([]bool, 12)
	for i := range ok {
		wg.Go(func() {
			_, err := w.d.Pool.Exec(t.Context(), submit, uuid.NewV7(), other, org, ibanFor(int64(900000+i)), evidence, "pending_verification", clubAdmin, false)
			ok[i] = err == nil
			if err != nil && db.SQLState(err) != db.CodeUniqueViolation {
				t.Errorf("concurrent submission: %v", err)
			}
		})
	}
	wg.Wait()
	if n := w.count(`SELECT count(*) FROM payee_bank_accounts WHERE payee_id = $1`, other); n != 1 {
		t.Errorf("concurrent submissions stored = %d, want 1", n)
	}
}

// TestIbanCheck_SQLMatchesGoVectors (T-42): every IBAN column of the P2
// schema accepts exactly the shared vectors that Go's iban.Valid accepts;
// a rejected value fails with the column's own CHECK (23514).
func TestIbanCheck_SQLMatchesGoVectors(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	s := w.shop()
	order, _ := w.order(s)
	w.markPaid(order)
	payment := w.payment(order, s.buyer.org)
	account := w.collectionAccount()
	org := w.org("club")
	payee := w.payee(org, w.legalParty("payee", "foundation", nil))
	evidence, user, other := w.file("bank_account_evidence"), w.user(), w.user()
	transfer := w.transfer(account, nil)
	columns := []struct {
		name, sql, constraint string
		args                  func(v string) []any
	}{
		{"payee_bank_accounts.iban", `INSERT INTO payee_bank_accounts (payee_id, org_id, iban, holder_name, evidence_file_id, status, submitted_by, submitted_by_admin)
			VALUES ($1, $2, $3, 'Vakıf', $4, 'pending_approval', $5, true)`, "payee_bank_accounts_iban_chk",
			func(v string) []any { return []any{payee, org, v, evidence, user} }},
		{"collection_accounts.iban", `INSERT INTO collection_accounts (label, legal_party_id, iban, holder_name, bank_name, created_by)
			VALUES ('Yedek', $1, $2, 'Koro', 'Banka', $3)`, "collection_accounts_iban_chk",
			func(v string) []any { return []any{w.platformParty(), v, user} }},
		{"incoming_bank_transfers.sender_iban", insertSQL("incoming_bank_transfers", transferDefaults, map[string]string{"sender_iban": "$5"}),
			"incoming_bank_transfers_sender_iban_chk",
			func(v string) []any { return []any{uuid.NewV7(), account, user, "V-" + v, v} }},
		{"incoming_bank_transfers.return_target_iban", `UPDATE incoming_bank_transfers SET status = 'return_requested', return_requested_by = $2,
			return_requested_at = now(), return_reason = 'Sipariş yok', return_target_iban = $3 WHERE id = $1`, "incoming_bank_transfers_return_target_iban_chk",
			func(v string) []any { return []any{transfer, user, v} }},
		{"refunds.target_iban", `INSERT INTO refunds (payment_id, order_id, amount, reason, requested_by, method, target_iban)
			VALUES ($1, $2, 1000, 'goodwill', $3, 'bank_eft', $4)`, "refunds_target_iban_chk",
			func(v string) []any { return []any{payment, order, other, v} }},
	}
	vectors := ibantest.Vectors()
	for _, v := range vectors {
		if iban.Valid(v.IBAN) != v.Valid {
			t.Errorf("Go iban.Valid(%q) = %v, want %v (%s)", v.IBAN, !v.Valid, v.Valid, v.Note)
		}
		for _, c := range columns {
			state, constraint := w.try(c.sql, c.args(v.IBAN)...)
			switch {
			case v.Valid && state != "":
				t.Errorf("%s rejects the valid %q (%s): %s %s", c.name, v.IBAN, v.Note, state, constraint)
			case !v.Valid && (state != db.CodeCheckViolation || constraint != c.constraint):
				t.Errorf("%s with the invalid %q (%s): SQLSTATE %q %q, want 23514 %s", c.name, v.IBAN, v.Note, state, constraint, c.constraint)
			}
		}
	}
	// payouts.iban_snapshot carries the same function (its value is also
	// bound to the approved account by payouts_bank_account_guard).
	if def := queryString(t, w.d, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'payouts_iban_snapshot_chk'`); def != "CHECK (koro_iban_tr_valid(iban_snapshot))" {
		t.Errorf("payouts_iban_snapshot_chk = %s", def)
	}
}
