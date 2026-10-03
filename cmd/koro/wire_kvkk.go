package main

import "github.com/danielgtaylor/huma/v2"

// kvkkWiring wires the kvkk module (L13; 01 §4.22). First owner: BE-021.
// Only tickets of the kvkk module edit this file (BE-001 R4).
// Reads c.privacy and c.sweepers; consumes hooks.talentAuditor (BE-021).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type kvkkWiring struct{}

func (w *kvkkWiring) build(c *container) error { return nil }

func (w *kvkkWiring) tasks(c *container) {}

func (w *kvkkWiring) routes(c *container, api huma.API) {}
