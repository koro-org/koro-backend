package main

import "github.com/danielgtaylor/huma/v2"

// entitlementsWiring wires the entitlements module (L2; 01 §4.4). First owner: BE-013.
// Only tickets of the entitlements module edit this file (BE-001 R4).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type entitlementsWiring struct{}

func (w *entitlementsWiring) build(c *container) error { return nil }

func (w *entitlementsWiring) tasks(c *container) {}

func (w *entitlementsWiring) routes(c *container, api huma.API) {}
