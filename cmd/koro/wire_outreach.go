package main

import "github.com/danielgtaylor/huma/v2"

// outreachWiring wires the outreach module (L8; 01 §4.13). First owner: BE-030.
// Only tickets of the outreach module edit this file (BE-001 R4).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type outreachWiring struct{}

func (w *outreachWiring) build(c *container) error { return nil }

func (w *outreachWiring) tasks(c *container) {}

func (w *outreachWiring) routes(c *container, api huma.API) {}
