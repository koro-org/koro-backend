package main

// useMemStore installs the in-memory payments stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-041.
// Only tickets of the payments module edit this file (BE-001 R4).
func (w *paymentsWiring) useMemStore() {}
