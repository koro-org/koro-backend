package config_test

import (
	"strings"
	"testing"
	"time"

	"koro/internal/platform/config"
)

type billingOnly struct {
	config.Common
	Billing config.Billing
}

// billingParasut is a complete set of Paraşüt settings (test values, no
// real account).
func billingParasut(env string, kv ...string) map[string]string {
	m := map[string]string{
		"APP_ENV": env, "BILLING_INVOICE_PROVIDER": "parasut", "BILLING_PARASUT_CLIENT_ID": "koro-test-client",
		"BILLING_PARASUT_CLIENT_SECRET": "test-client-secret-not-real",
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// TestBilling_Defaults: no invoice provider unless chosen, Paraşüt's
// production endpoints, series KRO, the basic scenario, 5 calls per 10 s.
func TestBilling_Defaults(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[billingOnly](map[string]string{"APP_ENV": "prod"})
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Billing
	p := b.Parasut
	if b.InvoiceProvider != config.BillingProviderNone || p.BaseURL != "https://api.parasut.com" ||
		p.TokenURL != "https://id.parasut.com/realms/parasut/protocol/openid-connect/token" || p.Series != "KRO" ||
		p.Scenario != "basic" || p.RequestsPer10s != 5 || p.Timeout != 10*time.Second {
		t.Fatalf("defaults = %+v", b)
	}
	if got := config.BillingInvoiceProviders(); strings.Join(got, ",") != "none,mem,parasut" {
		t.Fatalf("BillingInvoiceProviders = %v", got)
	}
}

// TestBilling_Validation pins BILLING_INVOICE_PROVIDER and the Paraşüt
// settings: mem only in dev, the client ID with the adapter, https in
// staging and prod, no client setting without the adapter; errors never
// repeat the secret.
func TestBilling_Validation(t *testing.T) {
	t.Parallel()
	ok := []map[string]string{
		{"APP_ENV": "dev", "BILLING_INVOICE_PROVIDER": "mem"},
		billingParasut("prod"),
		billingParasut("prod", "BILLING_PARASUT_CLIENT_SECRET", ""), // a public client
		billingParasut("staging", "BILLING_PARASUT_TOKEN_URL",
			"https://id-staging.parasut.com/realms/parasut/protocol/openid-connect/token", "BILLING_PARASUT_SCENARIO", "commercial"),
		billingParasut("dev", "BILLING_PARASUT_BASE_URL", "http://127.0.0.1:9000/", "BILLING_PARASUT_SERIES", "KRT1"),
	}
	for _, env := range ok {
		cfg, err := config.Load[billingOnly](env)
		if err != nil {
			t.Errorf("%v: %v", env, err)
		} else if strings.HasSuffix(cfg.Billing.Parasut.BaseURL, "/") {
			t.Errorf("%v: base URL keeps its slash", env)
		}
	}
	bad := map[string]map[string]string{
		"mem in prod":             {"APP_ENV": "prod", "BILLING_INVOICE_PROVIDER": "mem"},
		"unknown adapter":         {"APP_ENV": "dev", "BILLING_INVOICE_PROVIDER": "nilvera"},
		"no client ID":            billingParasut("prod", "BILLING_PARASUT_CLIENT_ID", " "),
		"client without adapter":  {"APP_ENV": "dev", "BILLING_PARASUT_CLIENT_SECRET": "test-client-secret-not-real"},
		"http in prod":            billingParasut("prod", "BILLING_PARASUT_BASE_URL", "http://api.parasut.com"),
		"credentials in token":    billingParasut("dev", "BILLING_PARASUT_TOKEN_URL", "https://u:p@id.parasut.com/token"),
		"bad series":              billingParasut("prod", "BILLING_PARASUT_SERIES", "KR-1"),
		"bad scenario":            billingParasut("prod", "BILLING_PARASUT_SCENARIO", "export"),
		"no budget":               billingParasut("prod", "BILLING_PARASUT_REQUESTS_PER_10S", "0"),
		"timeout":                 billingParasut("prod", "BILLING_PARASUT_TIMEOUT", "2m"),
		"long series":             billingParasut("prod", "BILLING_PARASUT_SERIES", "KORO0123456789ABC"),
		"query in base":           billingParasut("dev", "BILLING_PARASUT_BASE_URL", "https://api.parasut.com?x=1"),
		"unknown adapter, secret": {"APP_ENV": "dev", "BILLING_INVOICE_PROVIDER": "x", "BILLING_PARASUT_CLIENT_SECRET": "s"},
	}
	for name, env := range bad {
		_, err := config.Load[billingOnly](env)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), "test-client-secret-not-real") {
			t.Errorf("%s: the error repeats the secret: %v", name, err)
		}
	}
}
