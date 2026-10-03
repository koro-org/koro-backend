package main

import "github.com/danielgtaylor/huma/v2"

// auditWiring wires the audit module (L1; 01 §4.1). First owner: BE-007.
// Only tickets of the audit module edit this file (BE-001 R4).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type auditWiring struct{}

func (w *auditWiring) build(c *container) error { return nil }

func (w *auditWiring) tasks(c *container) {}

func (w *auditWiring) routes(c *container, api huma.API) {}
