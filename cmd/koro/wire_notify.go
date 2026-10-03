package main

import "github.com/danielgtaylor/huma/v2"

// notifyWiring wires the notify module (L3; 01 §4.5). First owner: BE-005.
// Only tickets of the notify module edit this file (BE-001 R4).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type notifyWiring struct{}

func (w *notifyWiring) build(c *container) error { return nil }

func (w *notifyWiring) tasks(c *container) {}

func (w *notifyWiring) routes(c *container, api huma.API) {}
