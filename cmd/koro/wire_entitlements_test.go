package main

// useMemStore installs the in-memory entitlements stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-013.
// Only tickets of the entitlements module edit this file (BE-001 R4).
func (w *entitlementsWiring) useMemStore() {}
