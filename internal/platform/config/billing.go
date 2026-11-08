package config

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// Billing configures the billing module (internal/billing). Only tickets of
// the billing module edit this file and the "# --- billing ---" block of
// .env.example (BE-001 R7).
type Billing struct {
	// InvoiceProvider selects the e-invoice adapter of the provider flow
	// (BILLING_INVOICE_PROVIDER; cmd/koro/wire_billing.go wires one per
	// value of BillingInvoiceProviders; BE-053):
	//   none     no adapter (default): every invoice is uploaded by finance
	//            or the payee, also under invoice_automation 'provider'.
	//   mem      the in-memory adapter (development and tests; refused in
	//            staging and prod).
	//   parasut  Paraşüt's API v4 with the BILLING_PARASUT_* settings; the
	//            OAuth tokens and the company ID are entered by an
	//            administrator (PUT /admin/integrations/parasut) and rotate
	//            every 30 minutes (billing.refresh_token).
	InvoiceProvider string `env:"BILLING_INVOICE_PROVIDER" envDefault:"none"`

	Parasut BillingParasut
}

// BillingParasut configures the Paraşüt adapter (BILLING_INVOICE_PROVIDER=parasut).
type BillingParasut struct {
	// BaseURL of Paraşüt's API (the adapter adds /v4/{company}).
	BaseURL string `env:"BILLING_PARASUT_BASE_URL" envDefault:"https://api.parasut.com"`
	// TokenURL is Paraşüt's OAuth token endpoint (staging:
	// id-staging.parasut.com).
	TokenURL string `env:"BILLING_PARASUT_TOKEN_URL" envDefault:"https://id.parasut.com/realms/parasut/protocol/openid-connect/token"`
	// ClientID and ClientSecret are Koro's API client at Paraşüt (the
	// secret is empty for a public client).
	ClientID     string `env:"BILLING_PARASUT_CLIENT_ID"`
	ClientSecret Secret `env:"BILLING_PARASUT_CLIENT_SECRET"`
	// Series is the invoice series Koro's provider invoices are reserved in
	// (invoices.reserved_series; Paraşüt's invoice_series).
	Series string `env:"BILLING_PARASUT_SERIES" envDefault:"KRO"`
	// Scenario is the e-Fatura scenario: basic (no answer from the
	// recipient) or commercial (the recipient may accept or reject).
	Scenario string `env:"BILLING_PARASUT_SCENARIO" envDefault:"basic"`
	// RequestsPer10s is the calls every worker together makes to Paraşüt
	// in 10 seconds (the shared rate limiter; Paraşüt's own limit depends on
	// the package).
	RequestsPer10s int `env:"BILLING_PARASUT_REQUESTS_PER_10S" envDefault:"5"`
	// Timeout bounds one request.
	Timeout time.Duration `env:"BILLING_PARASUT_TIMEOUT" envDefault:"10s"`
}

// Values of BILLING_INVOICE_PROVIDER.
const (
	BillingProviderNone    = "none"
	BillingProviderMem     = ProviderMem
	BillingProviderParasut = "parasut"
)

// BillingInvoiceProviders returns the accepted values of
// BILLING_INVOICE_PROVIDER (none has no adapter; cmd/koro wires every one).
func BillingInvoiceProviders() []string {
	return []string{BillingProviderNone, BillingProviderMem, BillingProviderParasut}
}

var reBillingSeries = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)

func (b *Billing) validate(c Common) error {
	var errs []error
	if err := checkProvider(c, "BILLING_INVOICE_PROVIDER", &b.InvoiceProvider, BillingInvoiceProviders()); err != nil {
		errs = append(errs, err)
	}
	p := &b.Parasut
	active := b.InvoiceProvider == BillingProviderParasut
	p.ClientID = strings.TrimSpace(p.ClientID)
	switch {
	case active && p.ClientID == "":
		errs = append(errs, errors.New("BILLING_INVOICE_PROVIDER=parasut needs BILLING_PARASUT_CLIENT_ID"))
	case !active && (p.ClientID != "" || !p.ClientSecret.IsZero()):
		errs = append(errs, errors.New("BILLING_PARASUT_CLIENT_ID and BILLING_PARASUT_CLIENT_SECRET need BILLING_INVOICE_PROVIDER=parasut"))
	}
	if err := checkAPIBaseURL(c, "BILLING_PARASUT_BASE_URL", &p.BaseURL, active); err != nil {
		errs = append(errs, err)
	}
	if err := checkAPIBaseURL(c, "BILLING_PARASUT_TOKEN_URL", &p.TokenURL, active); err != nil {
		errs = append(errs, err)
	}
	if !reBillingSeries.MatchString(p.Series) {
		errs = append(errs, errors.New("BILLING_PARASUT_SERIES must be 1-16 letters or digits"))
	}
	if p.Scenario != "basic" && p.Scenario != "commercial" {
		errs = append(errs, errors.New("BILLING_PARASUT_SCENARIO must be basic or commercial"))
	}
	if p.RequestsPer10s < 1 || p.RequestsPer10s > 1000 {
		errs = append(errs, errors.New("BILLING_PARASUT_REQUESTS_PER_10S must be between 1 and 1000"))
	}
	if p.Timeout <= 0 || p.Timeout > time.Minute {
		errs = append(errs, errors.New("BILLING_PARASUT_TIMEOUT must be positive and at most 1m"))
	}
	return errors.Join(errs...)
}
