package config

// Realtime configures Redis pub/sub and the WebSocket hub (internal/platform/realtime; 05 §10.7).
// Owner: BE-054 (BE-001 R7: only the owning ticket edits this file).
//
// The pub/sub bus uses the queue's Redis (REDIS_URL); the limits and timings
// are fixed by 04 §W (50 connections per event, 3 per user, re-authorisation
// every 60 s) and live in the realtime package.
type Realtime struct {
	// TrustedOrigins are the browser origins allowed to open a WebSocket
	// besides the API's own host. It is the same TRUSTED_ORIGINS variable as
	// CrossOriginProtection (HTTP): the handshake is a GET, which
	// CrossOriginProtection does not check, so the hub checks the Origin
	// against exactly the same list (Cross-Site WebSocket Hijacking). It is
	// read here as well so the hub, built with the modules, sees it; the
	// API validates it in HTTP.
	TrustedOrigins []string `env:"TRUSTED_ORIGINS" envSeparator:","`
}
