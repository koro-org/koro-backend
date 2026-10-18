//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/db/seed"
	"koro/internal/orders"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage/storagetest"
	"koro/internal/platform/testutil"
)

// The package already shares PostgreSQL and SeaweedFS. Only one commerce
// API/worker/Redis/Mailpit fixture runs at a time, including under -race and
// alongside the matrix: six independent tests must not multiply that load.
var commerceSlot = make(chan struct{}, 1)

// These are synthetic arithmetic and checksum vectors, not commercial terms
// or bank accounts. A booth costs 10000 + 2000 VAT; its illustrative 1000 bp
// commission and 200 VAT leave 10800 payable. Koro's quota costs 2500 + 500.
// The all-zero BBAN vector from ibantest stays distinct from seed.CollectionIBAN.
const commerceIBAN = "TR470000000000000000000000"
const commercePayeeIBAN = "TR039999901234567890123456"

type commerceEnv struct {
	t                                                       *testing.T
	d                                                       *db.DB
	c                                                       *smokeClient
	mail                                                    *mailpit
	environ                                                 map[string]string
	club, company, platform, party, payee, account, segment string
	adminID, finAID, finBID                                 string
	clubToken, buyer, admin, finA, finB                     string
	finATOTP, finBTOTP                                      *authenticator
	checkpoints                                             int
}

type commerceOrder struct {
	id, no, booth, native string
	amount                int64
}

func newCommerce(t *testing.T) *commerceEnv {
	t.Helper()
	select {
	case commerceSlot <- struct{}{}:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	t.Cleanup(func() { <-commerceSlot })
	d := testutil.NewDB(t)
	rdb := testutil.NewRedis(t)
	s3srv, s3 := storagetest.Start(t), storagetest.Client(t)
	smtpHost, smtpPort, mailAPI := startMailpit(t)
	env := seedEnviron(t, d)
	for k, v := range map[string]string{
		"HTTP_ADDR": "127.0.0.1:0", "PUBLIC_BASE_URL": "http://localhost:3000", "TRUSTED_ORIGINS": "http://localhost:3000",
		"SHUTDOWN_DELAY": "1ms", "SHUTDOWN_TIMEOUT": "5s", "RATE_LIMIT_ENABLED": "false", "REDIS_URL": redisURL(rdb),
		"WORKER_HEALTH_ADDR": "127.0.0.1:0", "WORKER_SHUTDOWN_TIMEOUT": "5s", "WORKER_SCHEDULER_ENABLED": "false",
		"WORKER_CONCURRENCY": "2", "OUTBOX_RELAY_IDLE": "50ms", "DB_MAX_CONNS": "4", "ORDERS_PAYMENT_WINDOW": "1h",
		"S3_ENDPOINT": s3srv.Endpoint, "S3_ACCESS_KEY": storagetest.AccessKey, "S3_SECRET_KEY": storagetest.SecretKey,
		"S3_REGION": storagetest.Region, "S3_BUCKET_FILES": storagetest.Bucket(t, s3), "S3_BUCKET_QUARANTINE": storagetest.Bucket(t, s3),
		"SMTP_HOST": smtpHost, "SMTP_PORT": strconv.Itoa(smtpPort), "SMTP_TLS": "none", "IDENTITY_LINK_BASE_URL": "http://localhost:3000",
	} {
		env[k] = v
	}
	seedCmd(t, env, "reference")
	seedCmd(t, env, "demo", "-password", demoPassword)
	api := startProcess(t, t.Context(), "api listening", "addr", func(ctx context.Context, stop context.CancelFunc, w io.Writer) error {
		return runAPI(ctx, stop, env, w)
	})
	startProcess(t, t.Context(), "worker started", "", func(ctx context.Context, stop context.CancelFunc, w io.Writer) error {
		return runWorker(ctx, stop, env, w)
	})
	e := &commerceEnv{t: t, d: d, environ: env, c: &smokeClient{t: t, ctx: t.Context(), base: "http://" + api, http: &http.Client{Timeout: 20 * time.Second}}, mail: &mailpit{t: t, ctx: t.Context(), api: mailAPI}}
	e.q(`SELECT m.org_id FROM users u JOIN org_members m ON m.user_id=u.id AND m.role='club_admin' WHERE u.email=$1`, &e.club, seed.ClubAdminEmail)
	e.q(`SELECT m.org_id FROM users u JOIN org_members m ON m.user_id=u.id AND m.role='company_owner' WHERE u.email=$1`, &e.company, seed.CompanyEmail)
	e.q(`SELECT id FROM organizations WHERE kind='platform'`, &e.platform)
	e.q(`SELECT id FROM users WHERE email=$1`, &e.adminID, seed.AdminEmail1)
	e.q(`SELECT id FROM users WHERE email=$1`, &e.finAID, seed.FinanceEmail1)
	e.q(`SELECT id FROM users WHERE email=$1`, &e.finBID, seed.FinanceEmail2)
	e.q(`SELECT id FROM segments WHERE org_id=$1 ORDER BY created_at,id LIMIT 1`, &e.segment, e.company)
	// Privileged demo accounts have no password (H-66). Exercise the actual
	// reset mail and MFA enrollment, then use opaque mobile sessions throughout.
	e.finA, e.finATOTP = e.privileged(seed.FinanceEmail1)
	e.finB, e.finBTOTP = e.privileged(seed.FinanceEmail2)
	e.admin, _ = e.privileged(seed.AdminEmail1)
	e.buyer = e.c.login(seed.CompanyEmail, demoPassword)
	e.c.enrolTOTP(e.buyer, demoPassword)
	e.clubToken = e.c.login(seed.ClubAdminEmail, demoPassword)
	// H-39: a platform admin's global role is distinct from membership of
	// Koro's seller organization. Native offerings require both.
	e.exec(`INSERT INTO org_members (org_id,org_kind,user_id,role,status,source,joined_at)
 VALUES ($1,'platform',$2,'staff','active','admin',now())`, e.platform, e.adminID)
	e.legalFixtures()
	// Demo supplies its own active default. This test creates a distinct
	// account through both eyes, then exercises the real default switch.
	if n := e.count(`SELECT count(*) FROM collection_accounts WHERE iban=$1 AND status='active' AND is_default`, seed.CollectionIBAN); n != 1 {
		t.Fatalf("seed default before fixture account: %d", n)
	}
	var platformParty string
	e.q(`SELECT id FROM legal_parties WHERE kind='platform'`, &platformParty)
	ac := e.post("/admin/collection-accounts", e.finA, map[string]any{"label": "Synthetic commerce fixture", "legal_party_id": platformParty, "iban": commerceIBAN, "holder_name": "Synthetic Platform", "bank_name": "Synthetic Bank", "reauth": e.proof(e.finATOTP)}, http.StatusCreated)
	e.account = ac["account_id"].(string)
	e.deny("/admin/collection-accounts/"+e.account+"/approve", e.finA, map[string]any{"decision": "approved"}, "authz.four_eyes_violation")
	e.post("/admin/collection-accounts/"+e.account+"/approve", e.finB, map[string]any{"decision": "approved", "reauth": e.proof(e.finBTOTP)}, http.StatusOK)
	e.post("/admin/collection-accounts/"+e.account+"/make-default", e.finA, map[string]any{"reauth": e.proof(e.finATOTP)}, http.StatusOK)
	if n := e.count(`SELECT count(*) FROM collection_accounts WHERE iban=$1 AND status='active' AND NOT is_default`, seed.CollectionIBAN); n != 1 {
		t.Fatalf("seed account after fixture default switch: %d", n)
	}
	e.balances(0, 0, 0, 0, 0)
	return e
}

func (e *commerceEnv) q(sql string, dst any, args ...any) {
	e.t.Helper()
	if err := e.d.Pool.QueryRow(e.t.Context(), sql, args...).Scan(dst); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}
func (e *commerceEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.d.Pool.Exec(e.t.Context(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}
func (e *commerceEnv) count(sql string, args ...any) int64 {
	e.t.Helper()
	var n int64
	e.q(sql, &n, args...)
	return n
}
func (e *commerceEnv) proof(a *authenticator) map[string]any {
	return map[string]any{"totp_code": a.code()}
}
func (e *commerceEnv) privileged(email string) (string, *authenticator) {
	e.t.Helper()
	e.c.do(http.MethodPost, "/api/v1/auth/password-resets", "", map[string]any{"email": email}, nil, http.StatusAccepted)
	mail := e.mail.wait(email, "Koro parolanızı sıfırlayın")
	match := linkToken.FindStringSubmatch(mail.Text)
	if len(match) != 2 {
		e.t.Fatal("password reset mail has no link token")
	}
	e.c.do(http.MethodPost, "/api/v1/auth/password-resets/confirm", "", map[string]any{"token": match[1], "new_password": demoPassword}, nil, http.StatusNoContent)
	token := e.c.login(email, demoPassword)
	return token, e.c.enrolTOTP(token, demoPassword)
}

// Only prerequisite legal identity is fixture SQL. Catalog, orders, payments,
// rights, delivery, payouts, uploads and invoices all use real HTTP handlers.
func (e *commerceEnv) legalFixtures() {
	e.t.Helper()
	e.party = uuid.NewV7().String()
	pp := uuid.NewV7().String()
	e.payee = uuid.NewV7().String()
	e.exec(`INSERT INTO legal_parties (id,kind,legal_type,org_id,title,tax_office,tax_no,address,email,status,verified_at,verified_by,created_by)
 VALUES ($1,'buyer_company','company',$2,'Synthetic Buyer','Test','1234567890','{"line1":"Test street","city":"Ankara","country":"TR"}',$3,'verified',now(),$4,$5)`, e.party, e.company, seed.CompanyEmail, e.finBID, e.adminID)
	e.exec(`INSERT INTO legal_parties (id,kind,legal_type,title,tax_office,tax_no,address,email,status,verified_at,verified_by,created_by)
 VALUES ($1,'payee','foundation','Synthetic Foundation','Test','9876543217','{"line1":"Test street","city":"Ankara","country":"TR"}',$2,'verified',now(),$3,$4)`, pp, seed.ClubAdminEmail, e.finBID, e.adminID)
	e.exec(`INSERT INTO legal_parties (kind,legal_type,title,tax_no,address,email,status,verified_at,verified_by,created_by)
 SELECT 'platform','company','Synthetic Platform','1234567890','{"line1":"Test street","city":"Ankara","country":"TR"}','platform@example.test','verified',now(),$1,$2 WHERE NOT EXISTS (SELECT 1 FROM legal_parties WHERE kind='platform')`, e.finBID, e.adminID)
	e.exec(`INSERT INTO payees (id,legal_party_id,org_id,status,invoicing_capability,verified_at,verified_by,created_by)
 VALUES ($1,$2,$3,'active','can_issue',now(),$4,$5)`, e.payee, pp, e.club, e.finBID, e.adminID)
	evidence := e.upload(e.finA, "bank_account_evidence")
	// An approved pre-existing seller bank account is a prerequisite. Its real
	// stored PDF and distinct submitter/approver preserve the DB four-eye checks.
	bank := uuid.NewV7().String()
	e.exec(`INSERT INTO payee_bank_accounts (id,payee_id,org_id,iban,holder_name,evidence_file_id,status,submitted_by,submitted_by_admin)
 VALUES ($1,$2,$3,$4,'Synthetic Foundation',$5,'pending_approval',$6,true)`, bank, e.payee, e.club, commercePayeeIBAN, evidence, e.finAID)
	e.exec(`UPDATE payee_bank_accounts SET status='approved',approved_by=$2,approved_at=now() WHERE id=$1`, bank, e.finBID)
}

func (e *commerceEnv) post(path, token string, body any, status int) map[string]any {
	e.t.Helper()
	out := e.c.do(http.MethodPost, "/api/v1"+path, token, body, map[string]string{"Idempotency-Key": uuid.NewV7().String()}, status)
	e.balanced(path)
	return out
}
func (e *commerceEnv) get(path, token string) map[string]any {
	e.t.Helper()
	return e.c.do(http.MethodGet, "/api/v1"+path, token, nil, nil, http.StatusOK)
}
func (e *commerceEnv) deny(path, token string, body any, code string) {
	e.t.Helper()
	out := e.post(path, token, body, http.StatusForbidden)
	if out["code"] != code {
		e.t.Fatalf("%s: code %v, want %s", path, out, code)
	}
}
func (e *commerceEnv) balanced(step string) {
	e.t.Helper()
	e.checkpoints++
	if n := e.count(`SELECT count(*) FROM (SELECT t.id FROM ledger_transactions t LEFT JOIN ledger_entries e ON e.transaction_id=t.id GROUP BY t.id HAVING count(e.id)<2 OR coalesce(sum(e.amount),0)<>0) bad`); n != 0 {
		e.t.Fatalf("%s: %d empty/unbalanced ledger transactions", step, n)
	}
	if n := e.count(`SELECT coalesce(sum(amount),0) FROM ledger_entries`); n != 0 {
		e.t.Fatalf("%s: ledger sum %d", step, n)
	}
}
func (e *commerceEnv) balances(bank, advances, payable, commission, sales int64) {
	e.t.Helper()
	e.balanced("account balances")
	for code, want := range map[string]int64{"koro_bank": bank, "buyer_advances": advances, "seller_payable": -payable, "platform_commission_revenue": -commission, "platform_sales_revenue": -sales, "vat_output": -(bank + advances - payable - commission - sales)} {
		if n := e.count(`SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE account_code=$1`, code); n != want {
			e.t.Errorf("%s balance=%d, want %d", code, n, want)
		}
	}
	if n := e.count(`SELECT -coalesce(sum(amount),0) FROM ledger_entries WHERE account_code='seller_payable' AND payee_id=$1`, e.payee); n != payable {
		e.t.Errorf("payee balance=%d, want %d", n, payable)
	}
}
func (e *commerceEnv) event(topic, aggregateID string) {
	e.t.Helper()
	waitUntil(e.t, topic+" relayed", func() bool {
		return e.count(`SELECT count(*) FROM outbox WHERE topic=$1 AND aggregate_id=$2 AND published_at IS NOT NULL`, topic, aggregateID) > 0
	})
}

func (e *commerceEnv) offering(native bool) string {
	e.t.Helper()
	kind, category, title, org, token, price := "org_bound", "booth", "Synthetic booth package", e.club, e.clubToken, 10000
	body := map[string]any{"code": "commerce_" + strings.ReplaceAll(uuid.NewV7().String(), "-", "")[:24], "name": title, "category": category, "fulfillment_type": kind, "accounting_reviewed": true, "accounting_reviewed_ref": "SYNTHETIC-TEST"}
	if native {
		org, token, price = e.platform, e.admin, 2500
		body["name"] = "Synthetic post quota"
		body["category"] = "targeted_post_quota"
		body["fulfillment_type"] = "platform_native"
		body["entitlement_template"] = map[string]any{"feature": "targeted_post", "kind": "quota", "quantity_per_unit": 2, "valid_days": 1}
	}
	service := e.post("/admin/services", e.finA, body, http.StatusCreated)["service_id"].(string)
	ob := map[string]any{"service_id": service, "title": title, "unit_price_amount": price, "quantity_total": 5, "deliverables": []any{map[string]any{"key": "test_package", "label": "Synthetic package"}}, "invoice_trigger": "on_fulfillment"}
	if native {
		ob["invoice_trigger"] = "on_payment"
	} else {
		ob["default_payee_id"] = e.payee
	}
	off := e.post("/orgs/"+org+"/offerings", token, ob, http.StatusCreated)["offering_id"].(string)
	// The immutable pricing policy is explicit per offering: no dependence on
	// bootstrap-commerce, production rates or another fixture's default policy.
	e.exec(`INSERT INTO commerce_policies (scope,scope_id,model_code,collection_method,commission_rate_bp,commission_basis,commission_payer,valid_from,created_by)
 VALUES ('offering',$1,'A','bank_transfer',1000,'net_excl_vat','seller',now()-interval '1 day',$2)`, off, e.adminID)
	if out := e.post("/offerings/"+off+"/submit", token, nil, http.StatusOK); out["status"] != "pending_approval" {
		e.t.Fatalf("submit: %v", out)
	}
	// A platform offering needs a different platform admin to approve it.
	approver := e.admin
	if native {
		approver, _ = e.privileged(seed.AdminEmail2)
		e.deny("/admin/offerings/"+off+"/decision", e.admin, map[string]any{"decision": "approved"}, "authz.four_eyes_violation")
	}
	if out := e.post("/admin/offerings/"+off+"/decision", approver, map[string]any{"decision": "approved"}, http.StatusOK); out["status"] != "active" {
		e.t.Fatalf("approval: %v", out)
	}
	e.balances(0, 0, 0, 0, 0)
	return off
}

func (e *commerceEnv) place(mixed bool) commerceOrder {
	e.t.Helper()
	booth := e.offering(false)
	lines := []any{map[string]any{"offering_id": booth, "quantity": 1}}
	native := ""
	if mixed {
		native = e.offering(true)
		lines = append(lines, map[string]any{"offering_id": native, "quantity": 1})
	}
	list := e.get("/offerings?limit=100", e.buyer)["items"].([]any)
	found := false
	for _, v := range list {
		m := v.(map[string]any)
		if m["offering_id"] == booth {
			found = true
			if _, ok := m["payee_id"]; ok {
				e.t.Fatal("catalog leaked the payee")
			}
		}
	}
	if !found {
		e.t.Fatal("approved offering is absent from buyer catalog")
	}
	body := map[string]any{"buyer_party_id": e.party, "lines": lines}
	key := map[string]string{"Idempotency-Key": uuid.NewV7().String()}
	out := e.c.do(http.MethodPost, "/api/v1/orgs/"+e.company+"/orders", e.buyer, body, key, http.StatusCreated)
	o := out["orders"].([]any)[0].(map[string]any)
	p := commerceOrder{id: o["order_id"].(string), no: o["order_no"].(string), amount: int64(o["payable_amount"].(float64))}
	if o["status"] != "awaiting_payment" || p.amount != 12000+int64(3000*boolInt(mixed)) {
		e.t.Fatalf("order: %v", o)
	}
	for _, v := range o["lines"].([]any) {
		l := v.(map[string]any)
		switch l["offering_id"] {
		case booth:
			p.booth = l["line_id"].(string)
		case native:
			p.native = l["line_id"].(string)
		}
		for _, field := range []string{"payee_id", "seller_net_amount", "commission_amount"} {
			if _, ok := l[field]; ok {
				e.t.Fatalf("buyer line leaked %s", field)
			}
		}
	}
	replay := e.c.do(http.MethodPost, "/api/v1/orgs/"+e.company+"/orders", e.buyer, body, key, http.StatusCreated)
	if replay["orders"].([]any)[0].(map[string]any)["order_id"] != p.id {
		e.t.Fatal("createOrder replay created another order")
	}
	if n := e.count(`SELECT count(*) FROM order_lines l JOIN offerings o ON o.id=l.offering_id WHERE l.order_id=$1 AND o.quantity_sold=1`, p.id); n != int64(len(lines)) {
		e.t.Fatalf("reserved offerings after create replay=%d, want %d", n, len(lines))
	}
	e.orderState(p, "awaiting_payment", 0, 0)
	instructions := e.get("/orders/"+p.id+"/payment-instructions", e.buyer)
	if instructions["reference_code"] != p.no || instructions["iban"] != commerceIBAN || instructions["amount_due_amount"] != float64(p.amount) {
		e.t.Fatalf("instructions: %v", instructions)
	}
	// Wait before capturing: payment instructions only apply to an unpaid order.
	mail := e.mail.wait(seed.CompanyEmail, "Siparişiniz alındı")
	if !strings.Contains(mail.Text, p.no) || !strings.Contains(mail.Text, commerceIBAN) {
		e.t.Fatalf("instructions mail missing reference/IBAN: %s", mail.Subject)
	}
	e.event("order.placed", p.id)
	if mixed {
		e.entitlement(p, "pending", 0)
	}
	e.balances(0, 0, 0, 0, 0)
	return p
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func (e *commerceEnv) orderState(p commerceOrder, status string, paid, refunded int64) {
	e.t.Helper()
	o := e.get("/orders/"+p.id, e.buyer)
	if o["status"] != status || o["paid_amount"] != float64(paid) || o["refunded_amount"] != float64(refunded) {
		e.t.Fatalf("order: %v; want %s paid=%d refunded=%d", o, status, paid, refunded)
	}
	e.balanced("order " + status)
}
func (e *commerceEnv) entitlement(p commerceOrder, status string, used int64) {
	e.t.Helper()
	if n := e.count(`SELECT count(*) FROM entitlements WHERE source_type='order_line' AND source_id=$1 AND status=$2 AND quantity_total=2 AND quantity_used=$3`, p.native, status, used); n != 1 {
		e.t.Fatalf("purchased entitlement is not %s, used=%d", status, used)
	}
}
func (e *commerceEnv) match(p commerceOrder, amount int64) map[string]any {
	e.t.Helper()
	tr := e.post("/admin/incoming-transfers", e.finA, map[string]any{"collection_account_id": e.account, "value_date": time.Now().UTC().Format("2006-01-02"), "amount": amount, "sender_name": "Synthetic Buyer", "description": p.no, "bank_reference": "SYNTHETIC-" + uuid.NewV7().String()}, http.StatusCreated)
	id := tr["transfer_id"].(string)
	if tr["status"] != "unmatched" || tr["extracted_order_no"] != p.no {
		e.t.Fatalf("transfer: %v", tr)
	}
	detail := e.get("/admin/incoming-transfers/"+id, e.finA)
	suggestions := detail["suggestions"].([]any)
	found := false
	for _, s := range suggestions {
		if s.(map[string]any)["order_id"] == p.id {
			found = true
		}
	}
	if !found {
		e.t.Fatalf("missing match suggestion: %v", detail)
	}
	body := map[string]any{"order_id": p.id, "expected_order_no": p.no, "refund_iban": commerceIBAN}
	headers := map[string]string{"Idempotency-Key": uuid.NewV7().String()}
	out := e.c.do(http.MethodPost, "/api/v1/admin/incoming-transfers/"+id+"/match", e.finA, body, headers, http.StatusOK)
	replay := e.c.do(http.MethodPost, "/api/v1/admin/incoming-transfers/"+id+"/match", e.finA, body, headers, http.StatusOK)
	payment := out["payment"].(map[string]any)
	if out["status"] != "matched" || payment["status"] != "captured" || payment["payment_id"] != replay["payment"].(map[string]any)["payment_id"] {
		e.t.Fatalf("capture/replay: %v / %v", out, replay)
	}
	e.event("payment.captured", payment["payment_id"].(string))
	e.balanced("capture and replay")
	return out
}

// upload exercises createUpload -> real presigned S3 PUT -> completeUpload
// -> outbox -> worker sanitization. Dev's explicit no-op scanner is allowed;
// there are no invented active files or URLs used as invoice evidence.
func (e *commerceEnv) upload(token, purpose string) string {
	e.t.Helper()
	content := commercePDF()
	body := map[string]any{"purpose": purpose, "filename": "synthetic.pdf", "content_type": "application/pdf", "size_bytes": len(content)}
	if token == e.clubToken {
		body["org_id"] = e.club
	}
	up := e.post("/files/uploads", token, body, http.StatusCreated)
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodPut, up["upload_url"].(string), bytes.NewReader(content))
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range up["upload_headers"].(map[string]any) {
		req.Header.Set(k, v.(string))
	}
	resp, err := e.c.http.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("S3 upload: %d %s", resp.StatusCode, raw)
	}
	id := up["file_id"].(string)
	e.post("/files/"+id+"/complete", token, map[string]any{"sha256": fmt.Sprintf("%x", sha256.Sum256(content))}, http.StatusOK)
	waitUntil(e.t, "stored PDF ready", func() bool {
		return e.count(`SELECT count(*) FROM files WHERE id=$1 AND status='active' AND scan_status IN ('clean','skipped')`, id) == 1
	})
	return id
}
func commercePDF() []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>"}
	offsets := make([]int, len(objects))
	for i, o := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

func TestCommerce_HappyPath_PackageToInvoice(t *testing.T) {
	t.Parallel()
	e := newCommerce(t)
	p := e.place(true)
	e.match(p, p.amount)
	e.orderState(p, "in_fulfilment", 15000, 0)
	e.entitlement(p, "active", 0)
	if n := e.count(`SELECT count(*) FROM order_lines WHERE id=$1 AND fulfillment_status='fulfilled' AND fulfilled_via='auto_platform'`, p.native); n != 1 {
		t.Fatal("paid native line was not automatically fulfilled")
	}
	if n := e.count(`SELECT coalesce(sum(seller_amount+platform_amount),0) FROM payment_splits WHERE order_id=$1 AND status='released'`, p.id); n != 15000 {
		t.Fatalf("released payment splits=%d, want 15000", n)
	}
	e.balances(15000, 0, 10800, 1000, 2500)
	tp := e.post("/orgs/"+e.company+"/targeted-posts", e.buyer, map[string]any{"kind": "text", "title": "Synthetic commerce announcement", "body_json": tiptap("Synthetic paid package announcement"), "commercial_flag": true, "audience": map[string]any{"segment_id": e.segment}}, http.StatusCreated)
	postID := tp["post_id"].(string)
	e.post("/posts/"+postID+"/publish", e.buyer, nil, http.StatusOK)
	e.entitlement(p, "active", 1)
	if n := e.count(`SELECT count(*) FROM entitlement_usages u JOIN entitlements en ON en.id=u.entitlement_id WHERE en.source_id=$1 AND u.ref_id=$2 AND u.reversal_of IS NULL`, p.native, postID); n != 1 {
		t.Fatal("targeted post did not consume its purchased entitlement")
	}
	e.post("/order-lines/"+p.booth+"/mark-fulfilled", e.clubToken, map[string]any{"note": "Synthetic delivery"}, http.StatusOK)
	if n := e.count(`SELECT count(*) FROM order_lines WHERE id=$1 AND fulfillment_status='marked' AND dispute_window_ends_at IS NOT NULL`, p.booth); n != 1 {
		t.Fatal("delivery was not marked with a dispute window")
	}
	e.post("/order-lines/"+p.booth+"/confirm", e.buyer, nil, http.StatusOK)
	e.event("order_line.fulfilled", p.booth)
	e.orderState(p, "completed", 15000, 0)
	e.balances(15000, 0, 10800, 1000, 2500)
	payout := e.post("/admin/payouts", e.finA, map[string]any{"payee_id": e.payee, "order_line_ids": []string{p.booth}}, http.StatusCreated)
	pid := payout["payout_id"].(string)
	if payout["amount"] != float64(10800) || payout["status"] != "pending_approval" {
		t.Fatalf("payout: %v", payout)
	}
	e.deny("/admin/payouts/"+pid+"/approve", e.finA, map[string]any{}, "authz.four_eyes_violation")
	e.post("/admin/payouts/"+pid+"/approve", e.finB, map[string]any{"reauth": e.proof(e.finBTOTP)}, http.StatusOK)
	receipt := e.upload(e.finA, "payout_receipt")
	e.post("/admin/payouts/"+pid+"/mark-paid", e.finA, map[string]any{"bank_reference": "SYNTHETIC-PAYOUT", "receipt_file_id": receipt}, http.StatusOK)
	e.event("payout.paid", pid)
	e.balances(4200, 0, 0, 1000, 2500)
	if n := e.count(`SELECT count(*) FROM order_lines WHERE id=$1 AND payout_id=$2 AND paid_out_at IS NOT NULL`, p.booth, pid); n != 1 {
		t.Fatal("payout did not mark the order line paid out")
	}
	view := e.get("/orgs/"+e.club+"/payouts/"+pid, e.clubToken)
	if _, ok := view["iban_snapshot"]; ok {
		t.Fatal("seller payout leaked the full IBAN")
	}
	if len(e.c.fetch(view["receipt_url"].(string))) == 0 {
		t.Fatal("empty payout receipt")
	}
	e.invoices(p, pid)
	t.Logf("%d HTTP/ledger checkpoints across catalog, payment, entitlement, fulfillment, payout and invoices", e.checkpoints)
}

func (e *commerceEnv) invoices(p commerceOrder, payoutID string) {
	e.t.Helper()
	waitUntil(e.t, "all invoice plans", func() bool {
		return e.count(`SELECT count(*) FROM invoices WHERE order_id=$1 OR payout_id=$2`, p.id, payoutID) == 3
	})
	rows, err := e.d.Pool.Query(e.t.Context(), `SELECT id,kind,issuer_org_id,net_amount,vat_amount FROM invoices WHERE order_id=$1 OR payout_id=$2 ORDER BY kind,id`, p.id, payoutID)
	if err != nil {
		e.t.Fatal(err)
	}
	type plan struct {
		id, kind string
		org      *string
		net, vat int64
	}
	var plans []plan
	for rows.Next() {
		var x plan
		if err := rows.Scan(&x.id, &x.kind, &x.org, &x.net, &x.vat); err != nil {
			e.t.Fatal(err)
		}
		plans = append(plans, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	for i, x := range plans {
		uploader, verifier := e.finA, e.finB
		if x.org != nil && *x.org == e.club {
			uploader, verifier = e.clubToken, e.finA
			if x.net != 10000 || x.vat != 2000 {
				e.t.Fatalf("seller invoice: %+v", x)
			}
		} else if x.kind == "commission" {
			if x.net != 1000 || x.vat != 200 {
				e.t.Fatalf("commission invoice: %+v", x)
			}
		} else if x.net != 2500 || x.vat != 500 {
			e.t.Fatalf("platform invoice: %+v", x)
		}
		file := e.upload(uploader, "invoice_pdf")
		e.post("/invoices/"+x.id+"/upload", uploader, map[string]any{"invoice_no": fmt.Sprintf("SYN2026%06d", i+1), "issue_date": time.Now().UTC().Format("2006-01-02"), "document_type": "e_arsiv", "pdf_file_id": file}, http.StatusOK)
		if uploader == e.finA {
			e.deny("/admin/invoices/"+x.id+"/verify", uploader, map[string]any{"result": "issued"}, "authz.four_eyes_violation")
		}
		e.post("/admin/invoices/"+x.id+"/verify", verifier, map[string]any{"result": "issued"}, http.StatusOK)
		e.event("invoice.issued", x.id)
		if n := e.count(`SELECT count(*) FROM invoices WHERE id=$1 AND status='issued' AND uploaded_by<>verified_by AND pdf_file_id=$2`, x.id, file); n != 1 {
			e.t.Fatal("invoice verification did not persist distinct eyes")
		}
		if x.kind == "service_sale" {
			link := e.get("/invoices/"+x.id+"/pdf", e.buyer)
			if !bytes.HasPrefix(e.c.fetch(link["url"].(string)), []byte("%PDF")) {
				e.t.Fatal("buyer invoice download is not PDF")
			}
		}
	}
	e.mail.wait(seed.CompanyEmail, "Koro: ")
	e.balances(4200, 0, 0, 1000, 2500)
}

func TestCommerce_PartialThenFullPayment(t *testing.T) {
	t.Parallel()
	e := newCommerce(t)
	p := e.place(true)
	e.match(p, 4000)
	e.orderState(p, "awaiting_payment", 4000, 0)
	e.entitlement(p, "pending", 0)
	e.balances(4000, -4000, 0, 0, 0)
	if n := e.count(`SELECT count(*) FROM ledger_transactions WHERE kind='order_allocated' AND ref_id=$1`, p.id); n != 0 {
		t.Fatal("partial payment allocated the order")
	}
	e.match(p, 11000)
	e.orderState(p, "in_fulfilment", 15000, 0)
	e.entitlement(p, "active", 0)
	if n := e.count(`SELECT count(*) FROM order_lines WHERE id=$1 AND fulfillment_status='fulfilled' AND fulfilled_via='auto_platform'`, p.native); n != 1 {
		t.Fatal("paid native line was not automatically fulfilled")
	}
	if n := e.count(`SELECT coalesce(sum(seller_amount+platform_amount),0) FROM payment_splits WHERE order_id=$1 AND status='released'`, p.id); n != 15000 {
		t.Fatalf("released payment splits=%d, want 15000", n)
	}
	e.balances(15000, 0, 10800, 1000, 2500)
	if n := e.count(`SELECT count(*) FROM ledger_transactions WHERE kind='order_allocated' AND ref_id=$1`, p.id); n != 1 {
		t.Fatalf("order allocations=%d, want 1", n)
	}
}

func (e *commerceEnv) refund(id string) {
	e.t.Helper()
	e.deny("/admin/refunds/"+id+"/approve", e.finA, map[string]any{}, "authz.four_eyes_violation")
	if n := e.count(`SELECT count(*) FROM refunds WHERE id=$1 AND status='requested' AND approved_by IS NULL`, id); n != 1 {
		e.t.Fatal("denied approval changed refund")
	}
	e.post("/admin/refunds/"+id+"/approve", e.finB, map[string]any{"reauth": e.proof(e.finBTOTP)}, http.StatusOK)
	file := e.upload(e.finA, "refund_receipt")
	body := map[string]any{"bank_reference": "SYNTHETIC-REFUND-" + id, "receipt_file_id": file}
	headers := map[string]string{"Idempotency-Key": uuid.NewV7().String()}
	path := "/api/v1/admin/refunds/" + id + "/complete"
	e.c.do(http.MethodPost, path, e.finA, body, headers, http.StatusOK)
	e.c.do(http.MethodPost, path, e.finA, body, headers, http.StatusOK)
	e.event("payment.refunded", id)
	e.balanced("refund completion and replay")
	if n := e.count(`SELECT count(*) FROM ledger_transactions WHERE kind='refund_completed' AND ref_id=$1`, id); n != 1 {
		e.t.Fatalf("refund transactions=%d", n)
	}
}
func TestCommerce_OverpaymentRefund_FourEyes(t *testing.T) {
	t.Parallel()
	e := newCommerce(t)
	p := e.place(false)
	out := e.match(p, 12500)
	id, ok := out["overpayment_refund_id"].(string)
	if !ok {
		t.Fatalf("overpayment has no refund: %v", out)
	}
	e.orderState(p, "paid", 12000, 0)
	e.balances(12500, -500, 10800, 1000, 0)
	if n := e.count(`SELECT count(*) FROM refunds WHERE id=$1 AND amount=500 AND reason='overpayment' AND order_line_id IS NULL AND target_iban=$2`, id, commerceIBAN); n != 1 {
		t.Fatal("overpayment refund pool or destination is wrong")
	}
	e.refund(id)
	e.orderState(p, "paid", 12000, 0)
	e.balances(12000, 0, 10800, 1000, 0)
	if n := e.count(`SELECT refunded_amount FROM payments WHERE id=$1`, out["payment"].(map[string]any)["payment_id"]); n != 500 {
		t.Fatalf("payment refunded=%d", n)
	}
}
func TestCommerce_OfferingCancelledAfterPayment_Refund(t *testing.T) {
	t.Parallel()
	e := newCommerce(t)
	p := e.place(false)
	capture := e.match(p, 12000)
	e.balances(12000, 0, 10800, 1000, 0)
	var off string
	e.q(`SELECT offering_id FROM order_lines WHERE id=$1`, &off, p.booth)
	e.post("/offerings/"+off+"/cancel", e.clubToken, map[string]any{"cancel_reason": "Synthetic cancellation"}, http.StatusOK)
	e.event("offering.cancelled", off)
	waitUntil(t, "paid line cancelled by outbox", func() bool {
		return e.count(`SELECT count(*) FROM order_lines WHERE id=$1 AND fulfillment_status='cancelled'`, p.booth) == 1
	})
	due := e.get("/admin/refunds/due", e.finA)
	found := false
	for _, v := range due["items"].([]any) {
		x := v.(map[string]any)
		if x["order_line_id"] == p.booth && x["amount"] == float64(12000) {
			found = true
		}
	}
	if !found {
		t.Fatalf("cancelled paid line missing from refund queue: %v", due)
	}
	r := e.post("/admin/refunds", e.finA, map[string]any{"payment_id": capture["payment"].(map[string]any)["payment_id"], "order_line_id": p.booth, "amount": 12000, "reason": "offering_cancelled", "target_iban": commerceIBAN}, http.StatusCreated)
	e.refund(r["refund_id"].(string))
	e.orderState(p, "refunded", 12000, 12000)
	e.balances(0, 0, 0, 0, 0)
}

// Expiry uses the production scheduled handler with its clock advanced, not
// an UPDATE to frozen order columns or a sleep lasting the payment window.
func TestCommerce_OrderExpires_ReleasesStockAndRights(t *testing.T) {
	t.Parallel()
	e := newCommerce(t)
	p := e.place(true)
	cfg, err := config.LoadAPI(e.environ)
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Now().UTC().Add(2 * time.Hour))
	database, err := openDB(t.Context(), cfg.Platform.DB, "commerce-expiry-test", clk, obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rdb, err := queue.OpenRedis(t.Context(), cfg.Platform.Queue.RedisURL.Reveal(), cfg.Platform.Queue.RedisPoolSize)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rdb.Close() }()
	c := newContainer(newInfra(infra{log: obs.Discard(), clock: clk, version: "test", cfg: cfg.Modules, db: database}, rdb, cfg.Platform))
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, shutdown := range c.shutdownHooks {
			shutdown()
		}
	}()
	found := false
	for _, j := range c.infra.jobs.Jobs() {
		if j.Name == orders.ExpireJobName {
			found = true
			if err := j.Handle(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := j.Handle(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found {
		t.Fatal("expiry job is not registered")
	}
	e.orderState(p, "expired", 0, 0)
	e.entitlement(p, "revoked", 0)
	waitUntil(t, "expiry notification delivered", func() bool {
		return e.count(`SELECT count(*) FROM in_app_notifications WHERE kind='orders.order_expired' AND data->>'order_id'=$1`, p.id) == 1
	})
	e.balances(0, 0, 0, 0, 0)
	if n := e.count(`SELECT count(*) FROM order_lines l JOIN offerings o ON o.id=l.offering_id WHERE l.order_id=$1 AND o.quantity_sold=0 AND l.fulfillment_status IN ('cancelled','not_applicable')`, p.id); n != 2 {
		t.Fatalf("released lines=%d, want 2", n)
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action='orders.order.expire' AND resource_id=$1`, p.id); n != 1 {
		t.Fatalf("expiry recorded %d audit events on replay", n)
	}
}

func TestCommerce_LedgerBalancedAtEveryStep(t *testing.T) {
	t.Parallel()
	e := newCommerce(t)
	p := e.place(false)
	capture := e.match(p, 4000)
	e.balances(4000, -4000, 0, 0, 0)
	e.post("/orders/"+p.id+"/cancel", e.buyer, map[string]any{"reason": "Synthetic partial-payment cancellation"}, http.StatusOK)
	e.orderState(p, "cancelled", 4000, 0)
	e.balances(4000, -4000, 0, 0, 0)
	r := e.post("/admin/refunds", e.finA, map[string]any{"payment_id": capture["payment"].(map[string]any)["payment_id"], "amount": 4000, "reason": "other", "reason_note": "Synthetic cancellation", "target_iban": commerceIBAN}, http.StatusCreated)
	e.balances(4000, -4000, 0, 0, 0)
	e.refund(r["refund_id"].(string))
	e.orderState(p, "cancelled", 4000, 4000)
	e.balances(0, 0, 0, 0, 0)
	if e.checkpoints < 20 {
		t.Fatalf("only %d checkpoints", e.checkpoints)
	}
	t.Logf("%d balanced checkpoints, including rejected approvals and idempotent replays", e.checkpoints)
}
