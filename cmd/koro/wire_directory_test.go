package main

// useMemStore installs the in-memory directory stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-020.
// Only tickets of the directory module edit this file (BE-001 R4).
func (w *directoryWiring) useMemStore() {}
