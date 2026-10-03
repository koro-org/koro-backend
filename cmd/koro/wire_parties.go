package main

import "github.com/danielgtaylor/huma/v2"

// partiesWiring wires the parties module (L7; 01 §4.12). First owner: BE-036.
// Only tickets of the parties module edit this file (BE-001 R4).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type partiesWiring struct{}

func (w *partiesWiring) build(c *container) error { return nil }

func (w *partiesWiring) tasks(c *container) {}

func (w *partiesWiring) routes(c *container, api huma.API) {}
