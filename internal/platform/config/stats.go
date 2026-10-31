package config

// Stats configures the stats module (internal/stats; BE-163). It has no
// setting: the panels' limits are code constants (stats.MaxRangeDays). Only
// tickets of the stats module edit this file and the "# --- stats ---"
// block of .env.example (BE-001 R7).
type Stats struct{}
