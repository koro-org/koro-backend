package main

import "github.com/danielgtaylor/huma/v2"

// filesWiring wires the files module (L2; 01 §4.2). First owner: BE-008.
// Only tickets of the files module edit this file (BE-001 R4).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type filesWiring struct{}

func (w *filesWiring) build(c *container) error { return nil }

func (w *filesWiring) tasks(c *container) {}

func (w *filesWiring) routes(c *container, api huma.API) {}
