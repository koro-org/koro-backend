package main

import "github.com/danielgtaylor/huma/v2"

// feedWiring wires the feed module (L9; 01 §4.14). First owner: BE-027.
// Only tickets of the feed module edit this file (BE-001 R4).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type feedWiring struct{}

func (w *feedWiring) build(c *container) error { return nil }

func (w *feedWiring) tasks(c *container) {}

func (w *feedWiring) routes(c *container, api huma.API) {}
