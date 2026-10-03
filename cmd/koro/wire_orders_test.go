package main

// useMemStore installs the in-memory orders stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-040.
// Only tickets of the orders module edit this file (BE-001 R4).
func (w *ordersWiring) useMemStore() {}
