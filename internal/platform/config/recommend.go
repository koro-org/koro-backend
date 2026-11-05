package config

// Recommend configures the recommend module (internal/recommend; BE-174).
// It has no setting: the rules' limits are code constants (recommend.
// DefaultLimit, MaxLimit, Horizon). Only tickets of the recommend module
// edit this file and the "# --- recommend ---" block of .env.example
// (BE-001 R7).
type Recommend struct{}
