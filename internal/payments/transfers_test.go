package payments_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/payments"
	"koro/internal/platform/civil"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// TestCreateTransfer: a transfer is recorded unmatched on an approved
// account with the reference code of its description; the same bank
// reference on the account is 409; a pending account, a future value date,
// a bad sender IBAN and a receipt of another purpose are 422; a platform
// admin cannot record (fin only).
func TestCreateTransfer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	fin := actor(t, "finance_admin")
	receipt := e.files.add(files.PurposeTransferReceipt)
	in := payments.TransferInput{CollectionAccountID: acct.ID, ValueDate: civil.From(t0), Amount: 1_200_000, SenderName: "ÖRNEK TEKNOLOJİ A.Ş.",
		SenderIBAN: "tr10 0006 2000 0000 0006 2959 38", Description: "kr7q 2m9t-xa stant ödemesi", BankReference: "20261004-000123", ReceiptFileID: &receipt}
	tr, err := e.svc.CreateTransfer(ctxOf(t, fin), fin, in)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != payments.TransferUnmatched || tr.ExtractedOrderNo == nil || *tr.ExtractedOrderNo != "KR7Q2M9TXA" ||
		tr.SenderIBAN == nil || *tr.SenderIBAN != ibanSender || tr.EntryMethod != payments.EntryManual || tr.EnteredBy != fin.UserID {
		t.Fatalf("transfer = %+v", tr)
	}
	if mod, owner := e.files.owner(receipt); mod != "payments" || owner != tr.ID {
		t.Fatalf("receipt attached to %s/%s", mod, owner)
	}
	in.ReceiptFileID = nil
	if _, err := e.svc.CreateTransfer(ctxOf(t, fin), fin, in); !errors.Is(err, payments.ErrDuplicateBankReference) {
		t.Fatalf("same reference = %v", err)
	}
	ev := e.audit.of(authz.PaymentsTransferCreate)
	if len(ev) != 1 || ev[0].Metadata["amount"] != int64(1_200_000) {
		t.Fatalf("audit = %+v", ev)
	}
	for _, s := range []string{"ÖRNEK", "TR10"} {
		if strings.Contains(fmt.Sprint(ev[0].Metadata), s) {
			t.Fatalf("audit metadata carries the sender's data: %v", ev[0].Metadata)
		}
	}
	pending := e.createAccount(t, "finance_admin", ibanKoro2)
	other := e.files.add(files.PurposeRefundReceipt)
	for name, mut := range map[string]func(*payments.TransferInput){
		"pending account": func(i *payments.TransferInput) { i.CollectionAccountID = pending.ID },
		"future date":     func(i *payments.TransferInput) { i.ValueDate = civil.From(t0).AddDays(2) },
		"zero amount":     func(i *payments.TransferInput) { i.Amount = 0 },
		"bad receipt":     func(i *payments.TransferInput) { i.ReceiptFileID = &other },
		"no description":  func(i *payments.TransferInput) { i.Description = "  " },
	} {
		bad := in
		bad.BankReference = "R-" + name
		mut(&bad)
		if _, err := e.svc.CreateTransfer(ctxOf(t, fin), fin, bad); !errors.Is(err, problem.ErrValidation) {
			t.Errorf("%s: %v, want 422", name, err)
		}
	}
	bad := in
	bad.BankReference, bad.SenderIBAN = "R-iban", "TR00"
	if _, err := e.svc.CreateTransfer(ctxOf(t, fin), fin, bad); !errors.Is(err, payments.ErrIBANInvalid) {
		t.Fatalf("bad sender IBAN = %v", err)
	}
	pad := actor(t, "platform_admin")
	if _, err := e.svc.CreateTransfer(ctxOf(t, pad), pad, in); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("platform admin records = %v", err)
	}
	// A retired account still takes transfers (money may arrive late).
	got, _ := e.store.GetAccount(t.Context(), acct.ID, false)
	if _, err := e.svc.RetireCollectionAccount(ctxOf(t, pad), pad, got.ID, good); err != nil {
		t.Fatal(err)
	}
	late := in
	late.BankReference = "late-1"
	if _, err := e.svc.CreateTransfer(ctxOf(t, fin), fin, late); err != nil {
		t.Fatalf("transfer on a retired account: %v", err)
	}
}

// TestListAndGetTransfers: newest value date first with a cursor, status /
// date / q filters (a reference code matches exactly), and the suggestion
// of an unmatched transfer rated by its amount.
func TestListAndGetTransfers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{})
	exact := e.transfer(t, acct, 1_200_000, "ödeme "+p.order.OrderNo, false)
	e.clk.Advance(24 * 3600e9)
	partial := e.transfer(t, acct, 400_000, p.order.OrderNo[:6]+" "+p.order.OrderNo[6:], false)
	plain := e.transfer(t, acct, 99, "faiz", false)
	pad := actor(t, "platform_admin")
	ctx := ctxOf(t, pad)

	pg, err := e.svc.ListTransfers(ctx, pad, payments.TransferQuery{Limit: 2})
	if err != nil || len(pg.Items) != 2 || pg.Next == nil || pg.Items[0].ValueDate != civil.From(e.clk.Now()) {
		t.Fatalf("page 1 = %+v, %v", pg, err)
	}
	pg2, err := e.svc.ListTransfers(ctx, pad, payments.TransferQuery{Limit: 2, After: pg.Next})
	if err != nil || len(pg2.Items) != 1 || pg2.Items[0].ID != exact.ID || pg2.Next != nil {
		t.Fatalf("page 2 = %+v, %v", pg2, err)
	}
	pg, err = e.svc.ListTransfers(ctx, pad, payments.TransferQuery{Filter: payments.TransferFilter{Q: strings.ToLower(p.order.OrderNo)}})
	if err != nil || len(pg.Items) != 2 {
		t.Fatalf("q=reference: %d, %v", len(pg.Items), err)
	}
	pg, err = e.svc.ListTransfers(ctx, pad, payments.TransferQuery{Filter: payments.TransferFilter{Q: "Faiz"}})
	if err != nil || len(pg.Items) != 1 || pg.Items[0].ID != plain.ID {
		t.Fatalf("q=faiz: %+v, %v", pg.Items, err)
	}
	day := civil.From(t0)
	pg, err = e.svc.ListTransfers(ctx, pad, payments.TransferQuery{Filter: payments.TransferFilter{ValueTo: &day}})
	if err != nil || len(pg.Items) != 1 || pg.Items[0].ID != exact.ID {
		t.Fatalf("value_date_to: %+v, %v", pg.Items, err)
	}
	if _, err := e.svc.ListTransfers(ctx, pad, payments.TransferQuery{Filter: payments.TransferFilter{Status: "lost"}}); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("bad status = %v", err)
	}

	for tr, want := range map[payments.IncomingTransfer]float64{exact: 1, partial: 0.7} {
		v, err := e.svc.GetTransfer(ctx, pad, tr.ID)
		if err != nil || len(v.Suggestions) != 1 || v.Suggestions[0].OrderID != p.order.ID || v.Suggestions[0].MatchConfidence != want ||
			v.Suggestions[0].AmountDue != 1_200_000 {
			t.Fatalf("suggestions of %d = %+v, %v", tr.Amount, v.Suggestions, err)
		}
	}
	if v, err := e.svc.GetTransfer(ctx, pad, plain.ID); err != nil || len(v.Suggestions) != 0 {
		t.Fatalf("no reference: %+v, %v", v.Suggestions, err)
	}
	stu := actor(t, "student")
	if _, err := e.svc.GetTransfer(ctxOf(t, stu), stu, exact.ID); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("student reads = %v", err)
	}
}

// TestIgnoreTransfer: only an unmatched transfer is ignored, with its
// reason audited; an ignored transfer is no longer suggested.
func TestIgnoreTransfer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	tr := e.transfer(t, acct, 50, "faiz", false)
	fin := actor(t, "finance_admin")
	got, err := e.svc.IgnoreTransfer(ctxOf(t, fin), fin, tr.ID, " Banka faizi ")
	if err != nil || got.Status != payments.TransferIgnored || got.IgnoreReason == nil || *got.IgnoreReason != "Banka faizi" {
		t.Fatalf("ignored = %+v, %v", got, err)
	}
	if _, err := e.svc.IgnoreTransfer(ctxOf(t, fin), fin, tr.ID, "tekrar"); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("ignore twice = %v", err)
	}
	if ev := e.audit.of(authz.PaymentsTransferIgnore); len(ev) != 1 || ev[0].Reason != "Banka faizi" {
		t.Fatalf("audit = %+v", ev)
	}
}

const statementHead = "\ufeffvalue_date;amount;description;bank_reference;sender_name;sender_iban\n"

// TestImportTransfers_DuplicateBankReference_SkippedRowLevel (04 §2.16): a
// row whose bank reference the account already has — recorded by hand or
// earlier in the same file — is skipped, a bad row is rejected with a
// reason, the others are imported (csv_import) with their reference codes.
func TestImportTransfers_DuplicateBankReference_SkippedRowLevel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	known := e.transfer(t, acct, 10_000, "elle girildi", false)
	csv := statementHead +
		"04.10.2026;1.200,00;KR7Q2M9TXA STANT;B-1;ÖRNEK A.Ş.;" + ibanSender + "\n" +
		"2026-10-04;250,5;Ödeme KR 7Q2M-9TXB;B-2;;\n" +
		"2026-10-04;100,00;elle girilmişin aynısı;" + known.BankReference + ";;\n" +
		"2026-10-04;1.200,00;dosyada iki kez;B-1;;\n" +
		"2026-10-04;-50,00;giden EFT;B-3;;\n" +
		"2026-13-04;10;tarih bozuk;B-4;;\n" +
		"2026-10-04;10;IBAN bozuk;B-5;X;TR00\n"
	fin := actor(t, "finance_admin")
	res, err := e.svc.ImportTransfers(ctxOf(t, fin), fin, payments.ImportInput{CollectionAccountID: acct.ID, CSV: csv})
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 2 || res.Duplicates != 2 || res.Rejected != 3 || len(res.Rows) != 7 {
		t.Fatalf("result = %+v", res)
	}
	want := []string{payments.RowImported, payments.RowImported, payments.RowDuplicate, payments.RowDuplicate, payments.RowRejected,
		payments.RowRejected, payments.RowRejected}
	for i, r := range res.Rows {
		if r.Outcome != want[i] || r.Line != i+2 {
			t.Errorf("row %d = %+v, want %s on line %d", i, r, want[i], i+2)
		}
		if r.Outcome == payments.RowRejected && (r.Error == "" || strings.Contains(r.Error, "TR00") || strings.Contains(r.Error, "-50")) {
			t.Errorf("row %d error %q", i, r.Error)
		}
	}
	first, err := e.store.GetTransfer(t.Context(), *res.Rows[0].TransferID, false)
	if err != nil || first.Amount != 120_000 || first.EntryMethod != payments.EntryCSVImport || first.ValueDate.String() != "2026-10-04" ||
		first.ExtractedOrderNo == nil || *first.ExtractedOrderNo != "KR7Q2M9TXA" || first.SenderIBAN == nil {
		t.Fatalf("first row = %+v, %v", first, err)
	}
	second, _ := e.store.GetTransfer(t.Context(), *res.Rows[1].TransferID, false)
	if second.Amount != 25_050 || second.ExtractedOrderNo == nil || *second.ExtractedOrderNo != "KR7Q2M9TXB" || second.SenderName != nil {
		t.Fatalf("second row = %+v", second)
	}
	if ev := e.audit.of(authz.PaymentsTransferCreate); len(ev) != 2 || ev[1].Metadata["imported"] != 2 || ev[1].Metadata["duplicates"] != 2 {
		t.Fatalf("audit = %+v", ev)
	}
	// A whole file that is not a statement is 422.
	for name, bad := range map[string]string{
		"no header":      "",
		"missing column": "value_date,amount,description\n2026-10-04,10,x\n",
		"no rows":        statementHead,
		"bad quoting":    "value_date,amount,description,bank_reference\n2026-10-04,10,\"x,B-9\n",
	} {
		if _, err := e.svc.ImportTransfers(ctxOf(t, fin), fin, payments.ImportInput{CollectionAccountID: acct.ID, CSV: bad}); !errors.Is(err, problem.ErrValidation) {
			t.Errorf("%s: %v, want 422", name, err)
		}
	}
}

// TestImportTransfers_IdempotencyReplay (06 H-5): the same key and
// statement replay the first answer (nothing is written again); the same
// key with another statement is 422 idempotency.key_reused; a new key
// re-imports nothing (row-level uniqueness).
func TestImportTransfers_IdempotencyReplay(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	fin := actor(t, "finance_admin")
	csv := "value_date,amount,description,bank_reference\n2026-10-04,\"1.200,00\",KR7Q2M9TXA,B-1\n2026-10-04,15.5,diğer,B-2\n"
	key := func(v string) idempotency.Key {
		return idempotency.Key{Scope: fin.UserID.String() + ":POST /admin/incoming-transfers/import", Value: v, Status: 202}
	}
	ctx := idempotency.WithKey(ctxOf(t, fin), key("import-key-1"))
	first, err := e.svc.ImportTransfers(ctx, fin, payments.ImportInput{CollectionAccountID: acct.ID, CSV: csv})
	if err != nil || first.Imported != 2 {
		t.Fatalf("first = %+v, %v", first, err)
	}
	again, err := e.svc.ImportTransfers(ctx, fin, payments.ImportInput{CollectionAccountID: acct.ID, CSV: csv})
	if err != nil || again.Imported != 2 || again.Duplicates != 0 || *again.Rows[0].TransferID != *first.Rows[0].TransferID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	pg, _ := e.store.ListTransfers(t.Context(), payments.TransferFilter{}, nil, 100)
	if len(pg) != 2 || len(e.audit.of(authz.PaymentsTransferCreate)) != 1 {
		t.Fatalf("a replay wrote again: %d transfers", len(pg))
	}
	if _, err := e.svc.ImportTransfers(ctx, fin, payments.ImportInput{CollectionAccountID: acct.ID, CSV: csv + "2026-10-04,1,x,B-3\n"}); !errors.Is(err, idempotency.ErrKeyReused) {
		t.Fatalf("same key, other statement = %v", err)
	}
	fresh, err := e.svc.ImportTransfers(idempotency.WithKey(ctxOf(t, fin), key("import-key-2")), fin,
		payments.ImportInput{CollectionAccountID: acct.ID, CSV: csv})
	if err != nil || fresh.Imported != 0 || fresh.Duplicates != 2 {
		t.Fatalf("new key = %+v, %v", fresh, err)
	}
}
