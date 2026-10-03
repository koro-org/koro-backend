package main

import "github.com/danielgtaylor/huma/v2"

// orgsWiring wires the orgs module (L5; 01 §4.7). First owner: BE-017.
// Only tickets of the orgs module edit this file (BE-001 R4).
// Consumes hooks.clubInfo (BE-019) and hooks.openCommerce (BE-017).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type orgsWiring struct{}

func (w *orgsWiring) build(c *container) error { return nil }

func (w *orgsWiring) tasks(c *container) {}

func (w *orgsWiring) routes(c *container, api huma.API) {}
