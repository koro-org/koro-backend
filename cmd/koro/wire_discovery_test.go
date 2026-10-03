package main

// useMemStore installs the in-memory discovery stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-051.
// Only tickets of the discovery module edit this file (BE-001 R4).
func (w *discoveryWiring) useMemStore() {}
