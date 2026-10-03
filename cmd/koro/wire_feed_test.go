package main

// useMemStore installs the in-memory feed stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-027.
// Only tickets of the feed module edit this file (BE-001 R4).
func (w *feedWiring) useMemStore() {}
