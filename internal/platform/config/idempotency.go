package config

// Idempotency configures Idempotency-Key handling (internal/platform/idempotency; 05 §10.4).
// Empty until BE-006 adds its fields (BE-001 R7: only the owning ticket edits this file).
type Idempotency struct{}
