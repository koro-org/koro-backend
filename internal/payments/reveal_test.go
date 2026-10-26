package payments_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/orders"
	"koro/internal/payments"
	"koro/internal/platform/civil"
	"koro/internal/platform/httpx"
	"koro/internal/platform/iban"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// newHTTP serves the payments operations of e with the persona resolver.
func newHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: authztest.NewResolver(authztest.Personas()...)})
	if err != nil {
		t.Fatal(err)
	}
	payments.Register(app.V1, e.svc)
	return app
}

// get calls GET path as persona with the headers hdr; a path of the form
// "POST <path>" is a read sent as POST (a …/search list, BE-128) with an
// empty JSON body.
func get(t *testing.T, app *httpx.App, persona, path string, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var req *http.Request
	if target, ok := strings.CutPrefix(path, http.MethodPost+" "); ok {
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if p, _ := authztest.Get(persona); p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// reason is an access reason as koro-web sends it (RFC 8187, BE-128).
func reason(s string) map[string]string {
	return map[string]string{httpx.HeaderAccessReason: "UTF-8''" + url.PathEscape(s)}
}

// revealFixture is a collection account, a transfer with a sender IBAN
// whose return is requested to ibanOther, and an overpayment refund back to
// the sender.
type revealFixture struct {
	acct   payments.CollectionAccount
	tr     payments.IncomingTransfer
	noIBAN payments.IncomingTransfer
	refund payments.Refund
}

func newRevealFixture(t *testing.T, e *env) revealFixture {
	t.Helper()
	acct := e.activeAccount(t, ibanKoro, true)
	fin := actor(t, "finance_admin")
	tr := e.transfer(t, acct, 77_000, "tanımsız", true)
	tr, err := e.svc.RequestReturn(ctxOf(t, fin), fin, tr.ID, payments.ReturnRequest{Reason: "Sipariş yok", TargetIBAN: ibanOther}, good)
	if err != nil {
		t.Fatal(err)
	}
	po := e.paid(t, acct, orderSpec{}, 0)
	r, err := e.request(t, "finance_admin", lineRefund(po.payment.ID, po.lines[0].ID, 100_000))
	if err != nil {
		t.Fatal(err)
	}
	return revealFixture{acct: acct, tr: tr, noIBAN: e.transfer(t, acct, 5_000, "göndericisiz", false), refund: r}
}

// TestRevealIBAN_AuditedWithReason (BE-129): each payments reveal hands a
// finance or platform admin one full IBAN with the access reason only
// (422 without it; 403 for anyone else, before anything is read; 404 for
// an IBAN the resource does not hold) and records it like a break-glass
// read: payments.iban.reveal, the reason, the field and the masked number,
// never the number.
func TestRevealIBAN_AuditedWithReason(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	fx := newRevealFixture(t, e)
	why := "İade EFT'si için banka ekranına yazılacak"
	reveals := []struct {
		name, field string
		resource    uuid.UUID
		want        string
		call        func(a authz.Actor, reason string) (string, error)
	}{
		{"collection account", "iban", fx.acct.ID, ibanKoro, func(a authz.Actor, r string) (string, error) {
			return e.svc.RevealCollectionAccountIBAN(ctxOf(t, a), a, fx.acct.ID, r)
		}},
		{"transfer sender", "sender_iban", fx.tr.ID, ibanSender, func(a authz.Actor, r string) (string, error) {
			return e.svc.RevealTransferIBAN(ctxOf(t, a), a, fx.tr.ID, payments.TransferIBANSender, r)
		}},
		{"transfer return target", "return.target_iban", fx.tr.ID, ibanOther, func(a authz.Actor, r string) (string, error) {
			return e.svc.RevealTransferIBAN(ctxOf(t, a), a, fx.tr.ID, payments.TransferIBANReturnTarget, r)
		}},
		{"refund", "target_iban", fx.refund.ID, ibanSender, func(a authz.Actor, r string) (string, error) {
			return e.svc.RevealRefundIBAN(ctxOf(t, a), a, fx.refund.ID, r)
		}},
	}
	for _, rv := range reveals {
		for _, name := range []string{"club_admin", "support", "dpo", "student"} {
			if _, err := rv.call(actor(t, name), why); !errors.Is(err, authz.ErrForbidden) {
				t.Errorf("%s as %s = %v, want 403", rv.name, name, err)
			}
		}
		for _, short := range []string{"", "neden", strings.Repeat("ğ", 501)} {
			_, err := rv.call(actor(t, "finance_admin"), short)
			problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
		}
	}
	if n := len(e.audit.Of(authz.PaymentsIbanReveal)); n != 0 {
		t.Fatalf("a refused reveal was audited (%d)", n)
	}
	for i, rv := range reveals {
		a := actor(t, []string{"finance_admin", "platform_admin"}[i%2])
		got, err := rv.call(a, why)
		if err != nil || got != rv.want {
			t.Fatalf("%s = %q, %v; want %s", rv.name, got, err, rv.want)
		}
		rs := e.audit.Of(authz.PaymentsIbanReveal)
		r := rs[len(rs)-1]
		if len(rs) != i+1 || r.Reason != why || r.ResourceID == nil || *r.ResourceID != rv.resource || r.ActorUserID == nil ||
			*r.ActorUserID != a.UserID || r.Metadata[audit.MetaBreakGlass] != true || r.Metadata["field"] != rv.field ||
			r.Metadata["iban_masked"] != iban.Mask(rv.want) {
			t.Fatalf("%s audit = %+v", rv.name, rs)
		}
	}
	fin := actor(t, "finance_admin")
	_, err := e.svc.RevealTransferIBAN(ctxOf(t, fin), fin, fx.noIBAN.ID, payments.TransferIBANSender, why)
	problemtest.WantErr(t, err, payments.ErrNotFound)
	_, err = e.svc.RevealTransferIBAN(ctxOf(t, fin), fin, fx.noIBAN.ID, payments.TransferIBANReturnTarget, why)
	problemtest.WantErr(t, err, payments.ErrNotFound)
	_, err = e.svc.RevealTransferIBAN(ctxOf(t, fin), fin, fx.tr.ID, "receiver", why)
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	_, err = e.svc.RevealRefundIBAN(ctxOf(t, fin), fin, uuid.NewV7(), why)
	problemtest.WantErr(t, err, payments.ErrNotFound)
	for _, ev := range e.audit.All() {
		if b, _ := json.Marshal(ev); strings.Contains(string(b), ibanSender) || strings.Contains(string(b), ibanOther) ||
			strings.Contains(string(b), ibanKoro) {
			t.Fatalf("an audit record holds a full IBAN: %s", b)
		}
	}
}

// TestHTTP_FinanceResponsesMaskIBAN (BE-129): no list or detail response of
// payments carries a full IBAN, each shows the masked field instead; the
// reveal operations answer the number (no-store) with the RFC 8187 access
// reason only; the refunds due name their currency.
func TestHTTP_FinanceResponsesMaskIBAN(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	fx := newRevealFixture(t, e)
	app := newHTTP(t, e)
	full := []string{ibanKoro, ibanSender, ibanOther}
	for _, path := range []string{
		"/api/v1/admin/collection-accounts",
		"POST /api/v1/admin/incoming-transfers/search",
		"/api/v1/admin/incoming-transfers/" + fx.tr.ID.String(),
		"/api/v1/admin/refunds",
		"/api/v1/admin/payments/" + fx.refund.PaymentID.String(),
	} {
		rec, _ := get(t, app, "finance_admin", path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		for _, n := range full {
			if strings.Contains(body, n) {
				t.Errorf("%s carries a full IBAN:\n%s", path, body)
			}
		}
		for _, field := range []string{`"iban"`, `"sender_iban"`, `"target_iban"`} {
			if strings.Contains(body, field) {
				t.Errorf("%s still has %s:\n%s", path, field, body)
			}
		}
	}
	_, out := get(t, app, "finance_admin", "/api/v1/admin/incoming-transfers/"+fx.tr.ID.String(), nil)
	ret, _ := out["return"].(map[string]any)
	if out["sender_iban_masked"] != iban.Mask(ibanSender) || ret["target_iban_masked"] != iban.Mask(ibanOther) {
		t.Fatalf("transfer masks = %v / %v", out["sender_iban_masked"], ret)
	}
	_, out = get(t, app, "platform_admin", "/api/v1/admin/collection-accounts", nil)
	if items, _ := out["items"].([]any); len(items) != 1 || items[0].(map[string]any)["iban_masked"] != iban.Mask(ibanKoro) {
		t.Fatalf("accounts = %v", out)
	}
	_, out = get(t, app, "finance_admin", "/api/v1/admin/refunds", nil)
	if items, _ := out["items"].([]any); len(items) != 1 || items[0].(map[string]any)["target_iban_masked"] != iban.Mask(ibanSender) {
		t.Fatalf("refunds = %v", out)
	}

	why := reason("Dört göz: banka mektubuyla karşılaştırma")
	for path, want := range map[string]string{
		"/api/v1/admin/collection-accounts/" + fx.acct.ID.String() + "/iban":                  ibanKoro,
		"/api/v1/admin/incoming-transfers/" + fx.tr.ID.String() + "/iban?field=sender":        ibanSender,
		"/api/v1/admin/incoming-transfers/" + fx.tr.ID.String() + "/iban?field=return_target": ibanOther,
		"/api/v1/admin/refunds/" + fx.refund.ID.String() + "/iban":                            ibanSender,
	} {
		rec, out := get(t, app, "finance_admin", path, why)
		if rec.Code != http.StatusOK || out["iban"] != want || out["iban_masked"] != iban.Mask(want) ||
			rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body)
		}
		if rec, _ := get(t, app, "finance_admin", path, nil); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s without a reason = %d", path, rec.Code)
		}
		if rec, _ := get(t, app, "club_admin", path, why); rec.Code != http.StatusForbidden {
			t.Errorf("%s as club_admin = %d", path, rec.Code)
		}
	}
	if rs := e.audit.Of(authz.PaymentsIbanReveal); len(rs) != 4 || rs[0].Reason != "Dört göz: banka mektubuyla karşılaştırma" {
		t.Fatalf("reveal audit = %+v", rs)
	}
	for _, q := range []string{"", "?field=receiver"} {
		if rec, _ := get(t, app, "finance_admin", "/api/v1/admin/incoming-transfers/"+fx.tr.ID.String()+"/iban"+q, why); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("transfer reveal %q = %d", q, rec.Code)
		}
	}

	// A cancelled line of a paid order: the refunds due carry the currency.
	po := e.paid(t, fx.acct, orderSpec{}, 0)
	l, _ := e.ordStore.GetLine(t.Context(), po.lines[0].ID, false)
	cancelled := l
	cancelled.FulfillmentStatus = orders.FulfillCancelled
	if _, err := e.ordStore.UpdateLine(t.Context(), cancelled, l.FulfillmentStatus); err != nil {
		t.Fatal(err)
	}
	rec, out := get(t, app, "finance_admin", "/api/v1/admin/refunds/due", nil)
	if items, _ := out["items"].([]any); rec.Code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["currency"] != "TRY" {
		t.Fatalf("refunds due = %d %s", rec.Code, rec.Body)
	}
}

// TestTransferReceipt_Download (BE-129): a finance or platform admin gets a
// 5-minute URL of the receipt recorded with a transfer, each download
// audited under payments.transfer.read; a transfer without a receipt is
// 404, a file not scanned clean 409, anyone else 403.
func TestTransferReceipt_Download(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	receipt := e.files.add(files.PurposeTransferReceipt)
	fin := actor(t, "finance_admin")
	tr, err := e.svc.CreateTransfer(ctxOf(t, fin), fin, payments.TransferInput{CollectionAccountID: acct.ID, ValueDate: civil.From(e.clk.Now()),
		Amount: 10_000, Description: "alacak tahsilatı", BankReference: "REF-RCPT-1", ReceiptFileID: &receipt})
	if err != nil {
		t.Fatal(err)
	}
	bare := e.transfer(t, acct, 5_000, "dekontsuz", false)
	pad := actor(t, "platform_admin")
	d, err := e.svc.TransferReceipt(ctxOf(t, pad), pad, tr.ID)
	if err != nil || !strings.HasSuffix(d.URL, receipt.String()) || d.ExpiresAt.IsZero() {
		t.Fatalf("receipt = %+v, %v", d, err)
	}
	reads := e.audit.Of(authz.PaymentsTransferRead)
	if len(reads) != 1 || reads[0].Metadata["operation"] != "download_receipt" || reads[0].Metadata["receipt_file_id"] != receipt.String() ||
		reads[0].Metadata["ttl_seconds"] != 300 || *reads[0].ResourceID != tr.ID {
		t.Fatalf("download audit = %+v", reads)
	}
	_, err = e.svc.TransferReceipt(ctxOf(t, fin), fin, bare.ID)
	problemtest.WantErr(t, err, payments.ErrNotFound)
	_, err = e.svc.TransferReceipt(ctxOf(t, fin), fin, uuid.NewV7())
	problemtest.WantErr(t, err, payments.ErrNotFound)
	cla := actor(t, "club_admin")
	_, err = e.svc.TransferReceipt(ctxOf(t, cla), cla, tr.ID)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	e.files.mu.Lock()
	m := e.files.m[receipt]
	m.ScanStatus = files.ScanPending
	e.files.m[receipt] = m
	e.files.mu.Unlock()
	_, err = e.svc.TransferReceipt(ctxOf(t, fin), fin, tr.ID)
	problemtest.WantErr(t, err, files.ErrNotClean)
	if n := len(e.audit.Of(authz.PaymentsTransferRead)); n != 1 {
		t.Fatalf("a refused download was audited (%d records)", n)
	}

	app := newHTTP(t, e)
	rec, out := get(t, app, "finance_admin", fmt.Sprintf("/api/v1/admin/incoming-transfers/%s/receipt", bare.ID), nil)
	if rec.Code != http.StatusNotFound || out["code"] != problem.ErrNotFound.Code() {
		t.Fatalf("receipt of a bare transfer = %d %s", rec.Code, rec.Body)
	}
}
