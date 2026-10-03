package main

// useMemStore installs the in-memory catalog stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-038.
// Only tickets of the catalog module edit this file (BE-001 R4).
func (w *catalogWiring) useMemStore() {}
