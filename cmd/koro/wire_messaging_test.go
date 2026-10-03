package main

// useMemStore installs the in-memory messaging stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-056.
// Only tickets of the messaging module edit this file (BE-001 R4).
func (w *messagingWiring) useMemStore() {}
