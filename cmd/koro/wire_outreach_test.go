package main

// useMemStore installs the in-memory outreach stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-030.
// Only tickets of the outreach module edit this file (BE-001 R4).
func (w *outreachWiring) useMemStore() {}
