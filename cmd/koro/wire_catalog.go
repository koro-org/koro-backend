package main

import "github.com/danielgtaylor/huma/v2"

// catalogWiring wires the catalog module (L9; 01 §4.17). First owner: BE-038.
// Only tickets of the catalog module edit this file (BE-001 R4).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type catalogWiring struct{}

func (w *catalogWiring) build(c *container) error { return nil }

func (w *catalogWiring) tasks(c *container) {}

func (w *catalogWiring) routes(c *container, api huma.API) {}
