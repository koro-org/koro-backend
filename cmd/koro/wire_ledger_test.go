package main

// useMemStore installs the in-memory ledger stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-039.
// Only tickets of the ledger module edit this file (BE-001 R4).
func (w *ledgerWiring) useMemStore() {}
