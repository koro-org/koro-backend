package main

// useMemStore installs the in-memory consent stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-012.
// Only tickets of the consent module edit this file (BE-001 R4).
func (w *consentWiring) useMemStore() {}
