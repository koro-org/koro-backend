package payments_test

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/payments"
	"koro/internal/platform/civil"
)

// contractWorld creates the parent rows a store needs (FKs in PostgreSQL;
// plain IDs in memory): users, Koro's own party and an order of a buyer
// with one line.
type contractWorld struct {
	user  func(t *testing.T) uuid.UUID
	party uuid.UUID
	order func(t *testing.T) contractOrder
}

type contractOrder struct{ id, org, line uuid.UUID }

func memContractWorld() contractWorld {
	return contractWorld{
		user:  func(*testing.T) uuid.UUID { return uuid.NewV7() },
		party: uuid.NewV7(),
		order: func(*testing.T) contractOrder { return contractOrder{uuid.NewV7(), uuid.NewV7(), uuid.NewV7()} },
	}
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (payments.Store, contractWorld) {
		return payments.NewMemStore(func() time.Time { return t0 }), memContractWorld()
	})
}

// storeContract pins the rules both stores apply (05 §13.2): the status
// guard of the targeted updates, the uniqueness rules and the four-eyes
// CHECKs of 000103/000105, the list order and the payment's ties.
func storeContract(t *testing.T, mk func(t *testing.T) (payments.Store, contractWorld)) {
	t.Run("accounts", func(t *testing.T) {
		t.Parallel()
		s, w := mk(t)
		ctx := t.Context()
		creator, approver := w.user(t), w.user(t)
		at := t0
		newAcct := func(number string) payments.CollectionAccount {
			return payments.CollectionAccount{ID: uuid.NewV7(), Label: "Ana", LegalPartyID: w.party, IBAN: number, HolderName: "Koro Teknoloji A.Ş.",
				BankName: "Örnek Bank", Status: payments.AccountPending, CreatedBy: creator, CreatedAt: at}
		}
		a, err := s.InsertAccount(ctx, newAcct(ibanKoro))
		if err != nil || a.Status != payments.AccountPending || a.PartyKind != payments.PartyKindPlatform {
			t.Fatalf("insert = %+v, %v", a, err)
		}
		if _, err := s.InsertAccount(ctx, newAcct(ibanKoro)); !errors.Is(err, payments.ErrIBANInUse) {
			t.Fatalf("same open IBAN = %v", err)
		}
		self := a
		self.Status, self.ApprovedBy, self.ApprovedAt = payments.AccountActive, &creator, &at
		if _, err := s.UpdateAccount(ctx, self, payments.AccountPending); !errors.Is(err, authz.ErrFourEyes) {
			t.Fatalf("self approval = %v", err)
		}
		next := a
		next.Status, next.ApprovedBy, next.ApprovedAt, next.IsDefault = payments.AccountActive, &approver, &at, true
		if _, err := s.UpdateAccount(ctx, next, payments.AccountRejected); !errors.Is(err, payments.ErrInvalidState) {
			t.Fatalf("wrong from = %v", err)
		}
		missing := next
		missing.ID = uuid.NewV7()
		if _, err := s.UpdateAccount(ctx, missing, payments.AccountPending); !errors.Is(err, payments.ErrNotFound) {
			t.Fatalf("missing row = %v", err)
		}
		a, err = s.UpdateAccount(ctx, next, payments.AccountPending)
		if err != nil || a.Status != payments.AccountActive || !a.IsDefault || a.ApprovedBy == nil || *a.ApprovedBy != approver {
			t.Fatalf("approve = %+v, %v", a, err)
		}
		// A second active default is refused (collection_accounts_default_uq).
		b, err := s.InsertAccount(ctx, newAcct(ibanKoro2))
		if err != nil {
			t.Fatal(err)
		}
		bn := b
		bn.Status, bn.ApprovedBy, bn.ApprovedAt, bn.IsDefault = payments.AccountActive, &approver, &at, true
		if _, err := s.UpdateAccount(ctx, bn, payments.AccountPending); !errors.Is(err, payments.ErrInvalidState) {
			t.Fatalf("second default = %v", err)
		}
		bn.IsDefault = false
		if _, err := s.UpdateAccount(ctx, bn, payments.AccountPending); err != nil {
			t.Fatal(err)
		}
		def, found, err := s.DefaultAccount(ctx)
		if err != nil || !found || def.ID != a.ID {
			t.Fatalf("default = %+v, %v, %v", def, found, err)
		}
		// Retired: no longer the default; its IBAN may be opened again.
		r := a
		r.Status, r.RetiredAt, r.IsDefault = payments.AccountRetired, &at, false
		if _, err := s.UpdateAccount(ctx, r, payments.AccountActive); err != nil {
			t.Fatal(err)
		}
		if _, found, _ := s.DefaultAccount(ctx); found {
			t.Fatal("a retired default")
		}
		if _, err := s.InsertAccount(ctx, newAcct(ibanKoro)); err != nil {
			t.Fatalf("IBAN of a retired account = %v", err)
		}
		list, err := s.ListAccounts(ctx)
		if err != nil || len(list) != 3 {
			t.Fatalf("list = %d, %v", len(list), err)
		}
		if got, err := s.GetAccount(ctx, a.ID, true); err != nil || got.Status != payments.AccountRetired {
			t.Fatalf("get = %+v, %v", got, err)
		}
		if _, err := s.GetAccount(ctx, uuid.NewV7(), false); !errors.Is(err, payments.ErrNotFound) {
			t.Fatalf("get missing = %v", err)
		}
	})

	t.Run("transfers", func(t *testing.T) {
		t.Parallel()
		s, w := mk(t)
		ctx := t.Context()
		acct := contractAccount(t, s, w, ibanKoro)
		acct2 := contractAccount(t, s, w, ibanKoro2)
		by := w.user(t)
		day := civil.From(t0)
		mkT := func(a payments.CollectionAccount, ref string, d civil.Date, created time.Time) payments.IncomingTransfer {
			return payments.IncomingTransfer{ID: uuid.NewV7(), CollectionAccountID: a.ID, ValueDate: d, Amount: 1000, Currency: "TRY",
				Description: "ödeme " + ref, BankReference: ref, Status: payments.TransferUnmatched, EntryMethod: payments.EntryManual,
				EnteredBy: by, CreatedAt: created}
		}
		t1, ok, err := s.InsertTransfer(ctx, mkT(acct, "R-1", day.AddDays(-2), t0.Add(-30*time.Hour)))
		if err != nil || !ok || t1.Status != payments.TransferUnmatched {
			t.Fatalf("insert = %+v, %v, %v", t1, ok, err)
		}
		if _, ok, err := s.InsertTransfer(ctx, mkT(acct, "R-1", day, t0)); err != nil || ok {
			t.Fatalf("same reference = %v, %v", ok, err)
		}
		if _, ok, err := s.InsertTransfer(ctx, mkT(acct2, "R-1", day, t0)); err != nil || !ok {
			t.Fatalf("same reference on another account = %v, %v", ok, err)
		}
		t3, _, _ := s.InsertTransfer(ctx, mkT(acct, "R-3", day, t0))
		ref := "KR7Q2M9TXA"
		t4 := mkT(acct, "R-4", day.AddDays(-1), t0)
		t4.ExtractedOrderNo, t4.SenderName = &ref, ptr("ÖRNEK A.Ş.")
		if t4, _, err = s.InsertTransfer(ctx, t4); err != nil {
			t.Fatal(err)
		}
		// Order: value date DESC, id DESC; the cursor continues after a row.
		all, err := s.ListTransfers(ctx, payments.TransferFilter{}, nil, 10)
		if err != nil || len(all) != 4 {
			t.Fatalf("list = %d, %v", len(all), err)
		}
		for i := 1; i < len(all); i++ {
			p, c := all[i-1], all[i]
			if c.ValueDate.After(p.ValueDate) || c.ValueDate == p.ValueDate && string(c.ID[:]) > string(p.ID[:]) {
				t.Fatalf("order broken at %d: %v/%s after %v/%s", i, p.ValueDate, p.ID, c.ValueDate, c.ID)
			}
		}
		rest, err := s.ListTransfers(ctx, payments.TransferFilter{}, &payments.TransferCursor{ValueDate: all[1].ValueDate, ID: all[1].ID}, 10)
		if err != nil || len(rest) != 2 || rest[0].ID != all[2].ID {
			t.Fatalf("after cursor = %+v, %v", rest, err)
		}
		from := day.AddDays(-1)
		for name, c := range map[string]struct {
			f    payments.TransferFilter
			want int
		}{
			"q description": {payments.TransferFilter{Q: "eme r-3"}, 1},
			"q sender":      {payments.TransferFilter{Q: "rnek a"}, 1},
			"q reference":   {payments.TransferFilter{Q: "kr7q2m9txa", QRef: ref}, 1},
			"q like chars":  {payments.TransferFilter{Q: "%"}, 0},
			"from":          {payments.TransferFilter{ValueFrom: &from}, 3},
			"to":            {payments.TransferFilter{ValueTo: &from}, 2},
			"status":        {payments.TransferFilter{Status: payments.TransferMatched}, 0},
		} {
			got, err := s.ListTransfers(ctx, c.f, nil, 10)
			if err != nil || len(got) != c.want {
				t.Errorf("%s: %d rows, %v; want %d", name, len(got), err, c.want)
			}
		}
		// Ignore, guard, return four eyes, reject clears, returned is final.
		ig := t3
		ig.Status, ig.IgnoreReason = payments.TransferIgnored, ptr("faiz")
		if _, err := s.UpdateTransfer(ctx, ig, payments.TransferMatched); !errors.Is(err, payments.ErrInvalidState) {
			t.Fatalf("wrong from = %v", err)
		}
		if _, err := s.UpdateTransfer(ctx, ig, payments.TransferUnmatched); err != nil {
			t.Fatal(err)
		}
		requester, approver, at := w.user(t), w.user(t), t0
		req := t1
		req.Status, req.ReturnRequestedBy, req.ReturnRequestedAt, req.ReturnReason, req.ReturnTargetIBAN =
			payments.TransferReturnRequested, &requester, &at, ptr("iade"), ptr(ibanSender)
		if t1, err = s.UpdateTransfer(ctx, req, payments.TransferUnmatched); err != nil {
			t.Fatal(err)
		}
		self := t1
		self.Status, self.ReturnApprovedBy, self.ReturnApprovedAt = payments.TransferReturnApproved, &requester, &at
		if _, err := s.UpdateTransfer(ctx, self, payments.TransferReturnRequested); !errors.Is(err, authz.ErrFourEyes) {
			t.Fatalf("self approval = %v", err)
		}
		rej := t1
		rej.Status = payments.TransferUnmatched
		rej.ReturnRequestedBy, rej.ReturnRequestedAt, rej.ReturnReason, rej.ReturnTargetIBAN = nil, nil, nil, nil
		if t1, err = s.UpdateTransfer(ctx, rej, payments.TransferReturnRequested); err != nil || t1.ReturnRequestedBy != nil {
			t.Fatalf("reject = %+v, %v", t1, err)
		}
		if t1, err = s.UpdateTransfer(ctx, req, payments.TransferUnmatched); err != nil {
			t.Fatal(err)
		}
		ok2 := t1
		ok2.Status, ok2.ReturnApprovedBy, ok2.ReturnApprovedAt = payments.TransferReturnApproved, &approver, &at
		if t1, err = s.UpdateTransfer(ctx, ok2, payments.TransferReturnRequested); err != nil {
			t.Fatal(err)
		}
		moved := t1
		moved.ReturnTargetIBAN = ptr(ibanOther)
		if _, err := s.UpdateTransfer(ctx, moved, payments.TransferReturnApproved); err == nil {
			t.Fatal("the target IBAN of an approved return changed")
		}
		if got, _ := s.GetTransfer(ctx, t1.ID, true); *got.ReturnTargetIBAN != ibanSender {
			t.Fatal("a refused update changed the row")
		}
		// Unmatched before: t4 (t0) is not older than t0−24h; t1 is in a return.
		sum, err := s.UnmatchedBefore(ctx, t0.Add(-24*time.Hour))
		if err != nil || sum.Count != 0 {
			t.Fatalf("unmatched = %+v, %v", sum, err)
		}
		sum, err = s.UnmatchedBefore(ctx, t0.Add(time.Hour))
		if err != nil || sum.Count != 2 || !sum.OldestAt.Equal(t0) {
			t.Fatalf("unmatched = %+v, %v", sum, err)
		}
		mine, err := s.TransfersOfUser(ctx, requester)
		if err != nil || len(mine) != 1 || mine[0].ID != t1.ID {
			t.Fatalf("transfers of the requester = %+v, %v", mine, err)
		}
		if mine, _ := s.TransfersOfUser(ctx, by); len(mine) != 4 {
			t.Fatalf("transfers of the recorder = %d", len(mine))
		}
	})

	t.Run("payments", func(t *testing.T) {
		t.Parallel()
		s, w := mk(t)
		ctx := t.Context()
		acct := contractAccount(t, s, w, ibanKoro)
		o := w.order(t)
		by, at := w.user(t), t0
		tr, _, err := s.InsertTransfer(ctx, payments.IncomingTransfer{ID: uuid.NewV7(), CollectionAccountID: acct.ID, ValueDate: civil.From(t0),
			Amount: 1_500_000, Currency: "TRY", Description: "x", BankReference: "P-1", Status: payments.TransferUnmatched,
			EntryMethod: payments.EntryManual, EnteredBy: by, CreatedAt: t0})
		if err != nil {
			t.Fatal(err)
		}
		pay := payments.Payment{ID: uuid.NewV7(), OrderID: o.id, OrgID: o.org, Provider: payments.ProviderBankTransfer, IncomingTransferID: &tr.ID,
			Amount: tr.Amount, Currency: "TRY", Status: payments.PaymentCaptured, CapturedAt: &at, CapturedBy: &by, CreatedAt: at}
		if err := s.InsertPayment(ctx, pay); err == nil {
			t.Fatal("a payment of an unmatched transfer (payments_incoming_transfer_id_order_id_fkey)")
		}
		m := tr
		m.Status, m.MatchedOrderID, m.MatchedBy, m.MatchedAt = payments.TransferMatched, &o.id, &by, &at
		if _, err := s.UpdateTransfer(ctx, m, payments.TransferUnmatched); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertPayment(ctx, pay); err != nil {
			t.Fatal(err)
		}
		again := pay
		again.ID = uuid.NewV7()
		if err := s.InsertPayment(ctx, again); !errors.Is(err, payments.ErrTransferAlreadyMatched) {
			t.Fatalf("second payment of the transfer = %v", err)
		}
		got, err := s.GetPayment(ctx, pay.ID)
		if err != nil || got.Amount != 1_500_000 || got.IncomingTransferID == nil || *got.IncomingTransferID != tr.ID {
			t.Fatalf("get = %+v, %v", got, err)
		}
		if _, err := s.GetPayment(ctx, uuid.NewV7()); !errors.Is(err, payments.ErrNotFound) {
			t.Fatalf("missing payment = %v", err)
		}
		sp := payments.Split{ID: uuid.NewV7(), PaymentID: pay.ID, OrderID: o.id, OrderLineID: o.line, SellerAmount: 1_080_000,
			PlatformAmount: 120_000, Status: payments.SplitReleased, CreatedAt: at}
		if err := s.InsertSplits(ctx, []payments.Split{sp}); err != nil {
			t.Fatal(err)
		}
		if ss, err := s.SplitsOf(ctx, pay.ID); err != nil || len(ss) != 1 || ss[0].SellerAmount != 1_080_000 {
			t.Fatalf("splits = %+v, %v", ss, err)
		}
		dup := sp
		dup.ID = uuid.NewV7()
		if err := s.InsertSplits(ctx, []payments.Split{dup}); err == nil {
			t.Fatal("a second split of the line")
		}
		if err := s.InsertRefund(ctx, payments.Refund{ID: uuid.NewV7(), PaymentID: pay.ID, OrderID: o.id, Amount: 300_000,
			Reason: payments.RefundReasonOverpayment, Status: payments.RefundStatusRequested, RequestedBy: by, Method: payments.RefundMethodBankEFT,
			TargetIBAN: ptr(ibanSender), CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertRefund(ctx, payments.Refund{ID: uuid.NewV7(), PaymentID: pay.ID, OrderID: o.id, Amount: 1,
			Reason: payments.RefundReasonOverpayment, Status: payments.RefundStatusRequested, RequestedBy: by, Method: payments.RefundMethodBankEFT,
			CreatedAt: at}); err == nil {
			t.Fatal("a bank EFT refund without a target IBAN")
		}
		if ps, err := s.PaymentsCapturedBy(ctx, by); err != nil || len(ps) != 1 {
			t.Fatalf("captured by = %+v, %v", ps, err)
		}
	})
}

// contractAccount inserts an active account of the world's party.
func contractAccount(t *testing.T, s payments.Store, w contractWorld, number string) payments.CollectionAccount {
	t.Helper()
	ctx := t.Context()
	c, err := s.InsertAccount(ctx, payments.CollectionAccount{ID: uuid.NewV7(), Label: "Ana", LegalPartyID: w.party, IBAN: number,
		HolderName: "Koro Teknoloji A.Ş.", BankName: "Örnek Bank", Status: payments.AccountPending, CreatedBy: w.user(t), CreatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	by, at := w.user(t), t0
	c.Status, c.ApprovedBy, c.ApprovedAt = payments.AccountActive, &by, &at
	if c, err = s.UpdateAccount(ctx, c, payments.AccountPending); err != nil {
		t.Fatal(err)
	}
	return c
}

func ptr[T any](v T) *T { return &v }
