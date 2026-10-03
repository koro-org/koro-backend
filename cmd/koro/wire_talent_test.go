package main

// useMemStore installs the in-memory talent stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-047.
// Only tickets of the talent module edit this file (BE-001 R4).
func (w *talentWiring) useMemStore() {}
