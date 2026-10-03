package main

import "github.com/danielgtaylor/huma/v2"

// studentsWiring wires the students module (L7; 01 §4.9). First owner: BE-023.
// Only tickets of the students module edit this file (BE-001 R4).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type studentsWiring struct{}

func (w *studentsWiring) build(c *container) error { return nil }

func (w *studentsWiring) tasks(c *container) {}

func (w *studentsWiring) routes(c *container, api huma.API) {}
