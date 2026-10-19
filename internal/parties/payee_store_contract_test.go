package parties_test

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/parties"
	"koro/internal/platform/problem"
)

func TestPayeeStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	payeeStoreContract(t, func(t *testing.T) (parties.Store, world) {
		s := parties.NewMemStore(func() time.Time { return t0 })
		return s, memWorld(s)
	})
}

// payeeStoreContract is the payees and payee_bank_accounts contract both
// stores pass (05 §13.3), with the constraints of 000096/000097 the
// service relies on: one open payee per (org, party), one active default,
// one pending and one approved account per payee, the initial status, the
// four-eyes CHECKs (answered 403 like authz), the state CHECKs and the
// guarded transitions.
func payeeStoreContract(t *testing.T, newStore func(t *testing.T) (parties.Store, world)) {
	t.Helper()
	ts := t0.Add(-time.Hour)
	stale := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC) // a caller's timestamp the stores ignore (BE-064)
	setup := func(t *testing.T) (parties.Store, world, uuid.UUID, parties.LegalParty) {
		t.Helper()
		s, w := newStore(t)
		vkn := vknC
		p, err := s.InsertParty(t.Context(), parties.LegalParty{ID: uuid.NewV7(), Kind: parties.KindPayee, LegalType: parties.LegalFoundation,
			Title: "Örnek Vakfı", TaxNo: &vkn, VATStatus: parties.VATUnknown, Status: parties.StatusPendingVerification})
		if err != nil {
			t.Fatal(err)
		}
		return s, w, w.org(t), p
	}
	newPayee := func(t *testing.T, s parties.Store, w world, org, party uuid.UUID, st parties.PayeeStatus, def bool) parties.Payee {
		t.Helper()
		p := parties.Payee{ID: uuid.NewV7(), LegalPartyID: party, OrgID: org, Status: st, IsDefault: def,
			PayoutMethod: parties.PayoutBankEFT, InvoicingCapability: parties.InvoicingUnknown, ValidFrom: ts, CreatedBy: w.user(t),
			CreatedAt: stale, UpdatedAt: stale}
		if st == parties.PayeeActive {
			by := w.user(t)
			p.VerifiedAt, p.VerifiedBy = &ts, &by
		}
		got, err := s.InsertPayee(t.Context(), p)
		if err != nil {
			t.Fatalf("InsertPayee: %v", err)
		}
		return got
	}
	newAccount := func(t *testing.T, s parties.Store, w world, py parties.Payee, number string, by uuid.UUID, admin bool) (parties.BankAccount, error) {
		t.Helper()
		st := parties.AccountPendingVerification
		if admin {
			st = parties.AccountPendingApproval
		}
		return s.InsertBankAccount(t.Context(), parties.BankAccount{ID: uuid.NewV7(), PayeeID: py.ID, OrgID: py.OrgID, IBAN: number,
			HolderName: "Örnek Vakfı", EvidenceFileID: w.file(t), Status: st, SubmittedBy: by, SubmittedByAdmin: admin, SubmittedAt: ts,
			CreatedAt: stale, UpdatedAt: stale})
	}
	isFourEyes := func(err error) bool { return errors.Is(err, authz.ErrFourEyes) }

	t.Run("payees: insert, scope, unique, update, default", func(t *testing.T) {
		t.Parallel()
		s, w, org, party := setup(t)
		ctx := t.Context()
		p := newPayee(t, s, w, org, party.ID, parties.PayeeDraft, true)
		if p.PartyKind != parties.KindPayee || p.CreatedAt.IsZero() || p.CreatedAt.Equal(stale) || !p.UpdatedAt.Equal(p.CreatedAt) ||
			!p.ValidFrom.Equal(ts) {
			t.Fatalf("InsertPayee = %+v", p)
		}
		if _, err := s.OrgPayee(ctx, org, p.ID, true); err != nil {
			t.Fatalf("OrgPayee: %v", err)
		}
		if _, err := s.OrgPayee(ctx, w.org(t), p.ID, false); !errors.Is(err, parties.ErrNotFound) {
			t.Fatalf("OrgPayee of another org: %v", err)
		}
		if _, err := s.GetPayee(ctx, uuid.NewV7(), false); !errors.Is(err, parties.ErrNotFound) {
			t.Fatalf("GetPayee of a missing row: %v", err)
		}
		dup := parties.Payee{ID: uuid.NewV7(), LegalPartyID: party.ID, OrgID: org, Status: parties.PayeeDraft,
			PayoutMethod: parties.PayoutBankEFT, InvoicingCapability: parties.InvoicingUnknown, ValidFrom: ts, CreatedBy: w.user(t)}
		if _, err := s.InsertPayee(ctx, dup); !errors.Is(err, parties.ErrPayeeExists) {
			t.Fatalf("second open payee: %v, want parties.payee_exists", err)
		}
		buyerOrg := w.org(t)
		vkn := vknA
		buyer, err := s.InsertParty(ctx, parties.LegalParty{ID: uuid.NewV7(), Kind: parties.KindBuyerCompany, LegalType: parties.LegalCompany,
			OrgID: &buyerOrg, Title: "Alıcı A.Ş.", TaxNo: &vkn, VATStatus: parties.VATUnknown, Status: parties.StatusPendingVerification})
		if err != nil {
			t.Fatal(err)
		}
		dup.ID, dup.LegalPartyID = uuid.NewV7(), buyer.ID
		if _, err := s.InsertPayee(ctx, dup); err == nil {
			t.Fatal("a buyer party became a payee (payees_legal_party_id_party_kind_fkey)")
		}
		// active needs its verification (payees_active_chk).
		next := p
		next.Status = parties.PayeeActive
		if _, err := s.UpdatePayee(ctx, next); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("active without verification: %v", err)
		}
		by := w.user(t)
		next.VerifiedAt, next.VerifiedBy = &ts, &by
		upd, err := s.UpdatePayee(ctx, next)
		if err != nil || upd.Status != parties.PayeeActive || !upd.UpdatedAt.After(p.UpdatedAt) || upd.UpdatedAt.Before(upd.CreatedAt) ||
			upd.CreatedBy != p.CreatedBy {
			t.Fatalf("UpdatePayee = %+v, %v", upd, err)
		}
		if def, ok, err := s.DefaultPayee(ctx, org); err != nil || !ok || def.ID != p.ID {
			t.Fatalf("DefaultPayee = %+v %v %v", def, ok, err)
		}
		// A second active default conflicts; ClearDefault moves it.
		vkn2 := vknD
		party2, err := s.InsertParty(ctx, parties.LegalParty{ID: uuid.NewV7(), Kind: parties.KindPayee, LegalType: parties.LegalFoundation,
			Title: "İkinci Vakıf", TaxNo: &vkn2, VATStatus: parties.VATUnknown, Status: parties.StatusPendingVerification})
		if err != nil {
			t.Fatal(err)
		}
		p2 := newPayee(t, s, w, org, party2.ID, parties.PayeeActive, false)
		p2.IsDefault = true
		if _, err := s.UpdatePayee(ctx, p2); err == nil {
			t.Fatal("two active defaults (payees_default_uq)")
		}
		cleared, err := s.ClearDefault(ctx, org, p2.ID)
		if err != nil || len(cleared) != 1 || cleared[0] != p.ID {
			t.Fatalf("ClearDefault = %v, %v", cleared, err)
		}
		if _, err := s.UpdatePayee(ctx, p2); err != nil {
			t.Fatalf("default after clearing: %v", err)
		}
		if def, _, _ := s.DefaultPayee(ctx, org); def.ID != p2.ID {
			t.Fatalf("default = %s", def.ID)
		}
		all, err := s.OrgPayees(ctx, org)
		if err != nil || len(all) != 2 || all[0].ID != p.ID {
			t.Fatalf("OrgPayees = %+v, %v", all, err)
		}
		created, err := s.PayeesCreatedBy(ctx, p.CreatedBy)
		if err != nil || len(created) != 1 || created[0].ID != p.ID {
			t.Fatalf("PayeesCreatedBy = %+v, %v", created, err)
		}
	})

	t.Run("bank accounts: four eyes, states, one pending and one approved", func(t *testing.T) {
		t.Parallel()
		s, w, org, party := setup(t)
		ctx := t.Context()
		py := newPayee(t, s, w, org, party.ID, parties.PayeeActive, true)
		clubAdmin, eyeA, eyeB := w.user(t), w.user(t), w.user(t)
		if _, err := newAccount(t, s, w, py, ibanBad, clubAdmin, false); !errors.Is(err, parties.ErrIBANInvalid) {
			t.Fatalf("invalid IBAN: %v, want parties.iban_invalid", err)
		}
		// A new row never starts approved (payee_bank_accounts_initial_status).
		if _, err := s.InsertBankAccount(ctx, parties.BankAccount{ID: uuid.NewV7(), PayeeID: py.ID, OrgID: py.OrgID, IBAN: ibanA,
			HolderName: "Örnek Vakfı", EvidenceFileID: w.file(t), Status: parties.AccountApproved, SubmittedBy: clubAdmin,
			SubmittedByAdmin: true, SubmittedAt: ts, ApprovedBy: &eyeA, ApprovedAt: &ts}); err == nil {
			t.Fatal("an account was inserted approved")
		}
		// The account's org is its payee's (payee_bank_accounts_payee_id_org_id_fkey).
		if _, err := s.InsertBankAccount(ctx, parties.BankAccount{ID: uuid.NewV7(), PayeeID: py.ID, OrgID: w.org(t), IBAN: ibanA,
			HolderName: "Örnek Vakfı", EvidenceFileID: w.file(t), Status: parties.AccountPendingVerification, SubmittedBy: clubAdmin,
			SubmittedAt: ts}); err == nil {
			t.Fatal("an account of another org was inserted")
		}
		b, err := newAccount(t, s, w, py, ibanA, clubAdmin, false)
		if err != nil || b.Status != parties.AccountPendingVerification || b.BankCode != "00061" || b.IBAN != ibanA ||
			b.CreatedAt.IsZero() || b.CreatedAt.Equal(stale) || !b.UpdatedAt.Equal(b.CreatedAt) {
			t.Fatalf("InsertBankAccount = %+v, %v", b, err)
		}
		if _, err := newAccount(t, s, w, py, ibanB, clubAdmin, false); !errors.Is(err, parties.ErrBankAccountPending) {
			t.Fatalf("second pending: %v, want parties.bank_account_pending", err)
		}
		// First eye = submitter: 403 (payee_bank_accounts_verifier_submitter_chk).
		v := b
		v.Status, v.VerifiedBy, v.VerifiedAt = parties.AccountPendingApproval, &clubAdmin, &ts
		if _, err := s.TransitionBankAccount(ctx, v, parties.AccountPendingVerification); !isFourEyes(err) {
			t.Fatalf("verifier = submitter: %v, want 403 authz.four_eyes_violation", err)
		}
		// An org submission does not skip its first eye (org_flow / approved CHECKs).
		skip := b
		skip.Status, skip.ApprovedBy, skip.ApprovedAt = parties.AccountApproved, &eyeB, &ts
		if _, err := s.TransitionBankAccount(ctx, skip, parties.AccountPendingVerification); err == nil {
			t.Fatal("an org submission was approved without its first eye")
		}
		v.VerifiedBy = &eyeA
		if b, err = s.TransitionBankAccount(ctx, v, parties.AccountPendingVerification); err != nil || b.Status != parties.AccountPendingApproval ||
			!b.UpdatedAt.After(b.CreatedAt) {
			t.Fatalf("verify = %+v, %v", b, err)
		}
		// A stale transition (the status moved on) is 409.
		if _, err := s.TransitionBankAccount(ctx, v, parties.AccountPendingVerification); !errors.Is(err, parties.ErrInvalidState) {
			t.Fatalf("stale transition: %v", err)
		}
		// Second eye = first eye, or = submitter: 403.
		for _, who := range []uuid.UUID{eyeA, clubAdmin} {
			a := b
			a.Status, a.ApprovedBy, a.ApprovedAt = parties.AccountApproved, &who, &ts
			if _, err := s.TransitionBankAccount(ctx, a, parties.AccountPendingApproval); !isFourEyes(err) {
				t.Fatalf("approver %s: %v, want 403", who, err)
			}
		}
		a := b
		a.Status, a.ApprovedBy, a.ApprovedAt = parties.AccountApproved, &eyeB, &ts
		if b, err = s.TransitionBankAccount(ctx, a, parties.AccountPendingApproval); err != nil || b.Status != parties.AccountApproved {
			t.Fatalf("approve = %+v, %v", b, err)
		}
		// A change: an admin submission (pending_approval), approved by another admin
		// once the old account is superseded by it (one approved account per payee).
		admin := w.user(t)
		c, err := newAccount(t, s, w, py, ibanB, admin, true)
		if err != nil || c.Status != parties.AccountPendingApproval {
			t.Fatalf("admin submission = %+v, %v", c, err)
		}
		ca := c
		ca.Status, ca.ApprovedBy, ca.ApprovedAt = parties.AccountApproved, &eyeB, &ts
		if _, err := s.TransitionBankAccount(ctx, ca, parties.AccountPendingApproval); err == nil {
			t.Fatal("two approved accounts (payee_bank_accounts_approved_uq)")
		}
		sup := b
		sup.Status, sup.SupersededAt, sup.SupersededByID = parties.AccountSuperseded, &ts, &c.ID
		if _, err := s.TransitionBankAccount(ctx, sup, parties.AccountApproved); err != nil {
			t.Fatalf("supersede: %v", err)
		}
		if c, err = s.TransitionBankAccount(ctx, ca, parties.AccountPendingApproval); err != nil {
			t.Fatalf("approve the change: %v", err)
		}
		m, err := s.ApprovedAccounts(ctx, []uuid.UUID{py.ID, uuid.NewV7()}, true)
		if err != nil || len(m) != 1 || m[py.ID].ID != c.ID {
			t.Fatalf("ApprovedAccounts = %+v, %v", m, err)
		}
		// A rejection carries its reason; a revocation too (06 H-37 one CHECK per state).
		d, err := newAccount(t, s, w, py, ibanC, clubAdmin, false)
		if err != nil {
			t.Fatal(err)
		}
		rej := d
		rej.Status, rej.RejectedBy, rej.RejectedAt = parties.AccountRejected, &eyeA, &ts
		if _, err := s.TransitionBankAccount(ctx, rej, parties.AccountPendingVerification); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("rejection without a reason: %v", err)
		}
		why := "Belge okunmuyor"
		rej.RejectionReason = &why
		if _, err := s.TransitionBankAccount(ctx, rej, parties.AccountPendingVerification); err != nil {
			t.Fatalf("reject: %v", err)
		}
		rev := c
		rev.Status, rev.RevokedBy, rev.RevokedAt, rev.RevokeReason = parties.AccountRevoked, &eyeA, &ts, &why
		if c, err = s.TransitionBankAccount(ctx, rev, parties.AccountApproved); err != nil || c.Status != parties.AccountRevoked ||
			c.IBAN != ibanB || c.ApprovedBy == nil {
			t.Fatalf("revoke = %+v, %v", c, err)
		}
		if m, _ := s.ApprovedAccounts(ctx, []uuid.UUID{py.ID}, false); len(m) != 0 {
			t.Fatal("a revoked account is still approved")
		}
		list, err := s.PayeeBankAccounts(ctx, py.ID)
		if err != nil || len(list) != 3 || list[0].ID != d.ID || list[2].ID != b.ID {
			t.Fatalf("PayeeBankAccounts (newest first) = %d, %v", len(list), err)
		}
		e, err := newAccount(t, s, w, py, ibanA, clubAdmin, false)
		if err != nil {
			t.Fatal(err)
		}
		q, err := s.ListBankAccounts(ctx, []parties.BankAccountStatus{parties.AccountPendingVerification, parties.AccountPendingApproval}, nil, 10)
		if err != nil || len(q) != 1 || q[0].ID != e.ID {
			t.Fatalf("queue = %+v, %v", q, err)
		}
		page1, _ := s.ListBankAccounts(ctx, nil, nil, 2)
		page2, _ := s.ListBankAccounts(ctx, nil, &page1[1].ID, 10)
		if len(page1) != 2 || page1[0].ID != b.ID || len(page2) != 2 || page2[1].ID != e.ID {
			t.Fatalf("pages = %d + %d", len(page1), len(page2))
		}
		mine, err := s.BankAccountsSubmittedBy(ctx, clubAdmin)
		if err != nil || len(mine) != 3 || mine[0].ID != b.ID {
			t.Fatalf("BankAccountsSubmittedBy = %d, %v", len(mine), err)
		}
	})
}
