package main

// useMemStore installs the in-memory files stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-008.
// Only tickets of the files module edit this file (BE-001 R4).
func (w *filesWiring) useMemStore() {}
