package config

// Consent configures the consent module (internal/consent). Owner: BE-012;
// only tickets of the consent module edit this file and the
// "# --- consent ---" block of .env.example (BE-001 R7).
type Consent struct {
	// IYSRequired refuses to start without a real İYS client (H-14: live
	// operation needs İYS; the adapter is BE-055). While false (dev, until
	// BE-055) submissions of commercial consents end as "skipped".
	IYSRequired bool `env:"CONSENT_IYS_REQUIRED" envDefault:"false"`
}
