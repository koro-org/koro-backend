package main

import "github.com/danielgtaylor/huma/v2"

// talentWiring wires the talent module (L8; 01 §4.11). First owner: BE-047.
// Only tickets of the talent module edit this file (BE-001 R4).
// Provides hooks.talentAuditor (BE-047/BE-052).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type talentWiring struct{}

func (w *talentWiring) build(c *container) error { return nil }

func (w *talentWiring) tasks(c *container) {}

func (w *talentWiring) routes(c *container, api huma.API) {}
