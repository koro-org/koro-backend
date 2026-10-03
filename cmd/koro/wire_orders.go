package main

import "github.com/danielgtaylor/huma/v2"

// ordersWiring wires the orders module (L10; 01 §4.18). First owner: BE-040.
// Only tickets of the orders module edit this file (BE-001 R4).
// Consumes hooks.collectionAccount; appends to hooks.openCommerce (BE-040).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type ordersWiring struct{}

func (w *ordersWiring) build(c *container) error { return nil }

func (w *ordersWiring) tasks(c *container) {}

func (w *ordersWiring) routes(c *container, api huma.API) {}
