package main

import "github.com/danielgtaylor/huma/v2"

// discoveryWiring wires the discovery module (L9; 01 §4.16). First owner: BE-051.
// Only tickets of the discovery module edit this file (BE-001 R4).
// Provides hooks.anonResolver (BE-051).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type discoveryWiring struct{}

func (w *discoveryWiring) build(c *container) error { return nil }

func (w *discoveryWiring) tasks(c *container) {}

func (w *discoveryWiring) routes(c *container, api huma.API) {}
