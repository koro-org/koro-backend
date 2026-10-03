package main

import "github.com/danielgtaylor/huma/v2"

// eventsWiring wires the events module (L8; 01 §4.10). First owner: BE-024.
// Only tickets of the events module edit this file (BE-001 R4).
// WebSocket hub shutdown goes to c.shutdownHooks (BE-054).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type eventsWiring struct{}

func (w *eventsWiring) build(c *container) error { return nil }

func (w *eventsWiring) tasks(c *container) {}

func (w *eventsWiring) routes(c *container, api huma.API) {}
