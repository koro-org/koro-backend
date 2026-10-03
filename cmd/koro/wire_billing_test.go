package main

// useMemStore installs the in-memory billing stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-042.
// Only tickets of the billing module edit this file (BE-001 R4).
func (w *billingWiring) useMemStore() {}
