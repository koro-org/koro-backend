package main

import "github.com/danielgtaylor/huma/v2"

// messagingWiring wires the messaging module (L10; 01 §4.15). First owner: BE-056.
// Only tickets of the messaging module edit this file (BE-001 R4).
// Consumes hooks.anonResolver (BE-056).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type messagingWiring struct{}

func (w *messagingWiring) build(c *container) error { return nil }

func (w *messagingWiring) tasks(c *container) {}

func (w *messagingWiring) routes(c *container, api huma.API) {}
