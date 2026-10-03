package config

// Audit configures the audit module (internal/audit).
// Empty until its first ticket (BE-007) adds fields; only tickets of the audit
// module edit this file and the "# --- audit ---" block of .env.example (BE-001 R7).
type Audit struct{}
