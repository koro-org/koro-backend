package main

import "github.com/danielgtaylor/huma/v2"

// paymentsWiring wires the payments module (L12; 01 §4.20). First owner: BE-041.
// Only tickets of the payments module edit this file (BE-001 R4).
// Provides hooks.collectionAccount (BE-041).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type paymentsWiring struct{}

func (w *paymentsWiring) build(c *container) error { return nil }

func (w *paymentsWiring) tasks(c *container) {}

func (w *paymentsWiring) routes(c *container, api huma.API) {}
