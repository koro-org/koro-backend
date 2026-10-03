package main

// useMemStore installs the in-memory kvkk stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-021.
// Only tickets of the kvkk module edit this file (BE-001 R4).
func (w *kvkkWiring) useMemStore() {}
