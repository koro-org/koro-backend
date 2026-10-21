package config

// Discovery configures the discovery module (internal/discovery). Only
// tickets of the discovery module edit this file and the
// "# --- discovery ---" block of .env.example (BE-001 R7).
//
// BE-051 needs no setting of its own: the pseudonyms (anon_id) and the
// session seeds of the ranking are derived from the token keyring
// (Crypto.TokenKeys, CRYPTO_TOKEN_KEYS) under the "discovery.anon_id" and
// "discovery.session_seed" purposes; the limits of a ranking are code
// constants (students.MaxDiscoveryMatches).
type Discovery struct{}
