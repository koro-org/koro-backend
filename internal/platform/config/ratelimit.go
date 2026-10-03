package config

// RateLimit configures rate limiting (internal/platform/ratelimit; 05 §10.5, 04 §1.9). API only.
// Empty until BE-006 adds its fields (BE-001 R7: only the owning ticket edits this file).
type RateLimit struct{}
