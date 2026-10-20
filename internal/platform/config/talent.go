package config

// Talent configures the talent module (internal/talent). Owner: BE-047;
// only tickets of the talent module edit this file and the
// "# --- talent ---" block of .env.example (BE-001 R7).
type Talent struct {
	// DefaultEnabled is the platform setting talent.default_enabled (T-32,
	// ADR-0015): while false no talent ledger row is written (every signal
	// consumer is a no-op, counted as koro_talent_ingest_total{outcome=
	// "flag_off"}) and every ranking band is neutral. It stays false until
	// the legal basis is decided and the privacy notice ("aktivite temelli
	// sıralama") is published; a student's activity_ranking_choice
	// "default" follows it. The neutralisations (opt-out, objection) are
	// recorded either way.
	DefaultEnabled bool `env:"TALENT_DEFAULT_ENABLED" envDefault:"false"`
}
