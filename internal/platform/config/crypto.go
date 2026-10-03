package config

// Crypto configures envelope encryption, blind indexes and derived tokens (internal/platform/crypto; local KEK from a Docker secret file).
// Empty until BE-003 adds its fields (BE-001 R7: only the owning ticket edits this file).
type Crypto struct{}
