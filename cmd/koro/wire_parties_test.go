package main

// useMemStore installs the in-memory parties stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-036.
// Only tickets of the parties module edit this file (BE-001 R4).
func (w *partiesWiring) useMemStore() {}
