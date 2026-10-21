package config

// Talent configures the talent module (internal/talent). Owner: BE-047;
// only tickets of the talent module edit this file and the
// "# --- talent ---" block of .env.example (BE-001 R7).
type Talent struct {
	// DefaultEnabled is the platform setting talent.default_enabled (T-32,
	// ADR-0015): while false no talent ledger row is written (every signal
	// consumer is a no-op, counted as koro_talent_ingest_total{outcome=
	// "flag_off"}), the nightly score (talent.recompute) does not run and
	// every ranking band is neutral. It stays false until the legal basis
	// is decided and the privacy notice ("aktivite temelli sıralama") is
	// published; a student's activity_ranking_choice "default" follows it.
	// The neutralisations (opt-out, objection) are recorded either way.
	DefaultEnabled bool `env:"TALENT_DEFAULT_ENABLED" envDefault:"false"`
	// AuditorAPIEnabled is the platform setting talent.auditor_api_enabled
	// (03 §5.12 k-flag, 04 §T, T-63; BE-052): while false every
	// talent_auditor endpoint (/admin/talent/**) is registered but answers
	// 404 to every signed-in user, before any authorisation; true opens
	// them to the talent_auditor role alone (never platform_admin, 03 Y1).
	AuditorAPIEnabled bool `env:"TALENT_AUDITOR_API_ENABLED" envDefault:"false"`
}
