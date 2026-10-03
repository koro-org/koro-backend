package main

import "github.com/danielgtaylor/huma/v2"

// billingWiring wires the billing module (L11; 01 §4.21). First owner: BE-042.
// Only tickets of the billing module edit this file (BE-001 R4).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type billingWiring struct{}

func (w *billingWiring) build(c *container) error { return nil }

func (w *billingWiring) tasks(c *container) {}

func (w *billingWiring) routes(c *container, api huma.API) {}
