//go:build integration

package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"koro/db/seed"
	"koro/internal/platform/testutil"
)

// BE-065 exercises the real seed command, authentication and API dependency
// wiring. No SQL fixture supplies a bank account or changes the seeded order.
func TestSeedDemo_CollectionPaymentInstructions(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	env := seedEnviron(t, d)
	seedCmd(t, env, "reference")
	seedCmd(t, env, "demo", "-password", demoPassword)
	seedCmd(t, env, "demo", "-password", demoPassword)
	for k, v := range map[string]string{
		"HTTP_ADDR": "127.0.0.1:0", "PUBLIC_BASE_URL": "http://localhost:3000", "TRUSTED_ORIGINS": "http://localhost:3000",
		"SHUTDOWN_DELAY": "1ms", "SHUTDOWN_TIMEOUT": "5s", "RATE_LIMIT_ENABLED": "false", "REDIS_URL": redisURL(testutil.NewRedis(t)),
	} {
		env[k] = v
	}
	addr := startProcess(t, t.Context(), "api listening", "addr", func(ctx context.Context, stop context.CancelFunc, w io.Writer) error {
		return runAPI(ctx, stop, env, w)
	})
	c := &smokeClient{t: t, ctx: t.Context(), base: "http://" + addr, http: &http.Client{Timeout: 20 * time.Second}}
	buyer := c.login(seed.CompanyEmail, demoPassword)
	c.enrolTOTP(buyer, demoPassword)
	var order string
	if err := d.Pool.QueryRow(t.Context(), `SELECT id FROM orders WHERE order_no = 'KRDEM00000'`).Scan(&order); err != nil {
		t.Fatal(err)
	}
	instructions := c.do(http.MethodGet, "/api/v1/orders/"+order+"/payment-instructions", buyer, nil, nil, http.StatusOK)
	if instructions["reference_code"] != "KRDEM00000" || instructions["iban"] != seed.CollectionIBAN || instructions["amount_due_amount"] != float64(12000) {
		t.Fatalf("seeded payment instructions: %v", instructions)
	}
	holder, _ := instructions["holder_name"].(string)
	bank, _ := instructions["bank_name"].(string)
	if !strings.HasPrefix(holder, "ÖRNEK") || !strings.HasPrefix(bank, "ÖRNEK") {
		t.Fatalf("not visibly synthetic: %v", instructions)
	}
	var n int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM orders o JOIN order_lines l ON l.order_id = o.id
 JOIN entitlements e ON e.source_id = l.id AND e.source_type = 'order_line'
 WHERE o.id = $1 AND o.status = 'awaiting_payment' AND o.paid_amount = 0 AND o.paid_at IS NULL
 AND o.collection_account_id IS NULL AND e.status = 'pending' AND e.activated_at IS NULL`, order).Scan(&n); err != nil || n != 1 {
		t.Fatalf("instructions preserve unpaid order/pending entitlement: %d (%v)", n, err)
	}
	// The Koro account does not substitute for Model C's separately approved payee IBAN.
	var directOrder string
	if err := d.Pool.QueryRow(t.Context(), `SELECT id FROM orders WHERE order_no = 'KRDEM00003'`).Scan(&directOrder); err != nil {
		t.Fatal(err)
	}
	problem := c.do(http.MethodGet, "/api/v1/orders/"+directOrder+"/payment-instructions", buyer, nil, nil, http.StatusConflict)
	if problem["code"] != "parties.no_approved_account" {
		t.Fatalf("Model C separate account requirement: %v", problem)
	}
	for _, table := range []string{"payee_bank_accounts", "payments", "incoming_bank_transfers", "ledger_transactions", "ledger_entries", "invoices"} {
		if err := d.Pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("unexpected %s history: %d (%v)", table, n, err)
		}
	}
}
