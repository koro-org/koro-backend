package main

// useMemStore installs the in-memory students stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-023.
// Only tickets of the students module edit this file (BE-001 R4).
func (w *studentsWiring) useMemStore() {}
