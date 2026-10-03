package config

// DB configures the database pool and statement timeouts (internal/platform/db; 05 §6.1, §12.1: DATABASE_URL, DB_MAX_CONNS, DB_STATEMENT_TIMEOUT).
// Empty until BE-002 adds its fields (BE-001 R7: only the owning ticket edits this file).
type DB struct{}
