package main

// useMemStore installs the in-memory identity stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-014.
// Only tickets of the identity module edit this file (BE-001 R4).
func (w *identityWiring) useMemStore() {}
