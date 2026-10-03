package main

// useMemStore installs the in-memory events stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-024.
// Only tickets of the events module edit this file (BE-001 R4).
func (w *eventsWiring) useMemStore() {}
