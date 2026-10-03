package main

// useMemStore installs the in-memory orgs stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-017.
// Only tickets of the orgs module edit this file (BE-001 R4).
func (w *orgsWiring) useMemStore() {}
