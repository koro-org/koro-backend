package main

import "github.com/danielgtaylor/huma/v2"

// identityWiring wires the identity module (L4; 01 §4.6). First owner: BE-014.
// Only tickets of the identity module edit this file (BE-001 R4).
// Consumes hooks.domainResolver (BE-018); sets c.actors via cmd/koro/actor.go (BE-014).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type identityWiring struct{}

func (w *identityWiring) build(c *container) error { return nil }

func (w *identityWiring) tasks(c *container) {}

func (w *identityWiring) routes(c *container, api huma.API) {}
