package main

// useMemStore installs the in-memory audit stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-007.
// Only tickets of the audit module edit this file (BE-001 R4).
func (w *auditWiring) useMemStore() {}
