package main

// useMemStore installs the in-memory notify stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-005.
// Only tickets of the notify module edit this file (BE-001 R4).
func (w *notifyWiring) useMemStore() {}
