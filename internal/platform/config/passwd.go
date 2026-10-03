package config

// Passwd configures password hashing (internal/platform/passwd; 05 §14.1: PASSWORD_PEPPER). API and seed only.
// Empty until BE-003 adds its fields (BE-001 R7: only the owning ticket edits this file).
type Passwd struct{}
