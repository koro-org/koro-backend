package main

import "github.com/danielgtaylor/huma/v2"

// consentWiring wires the consent module (L2; 01 §4.3). First owner: BE-012.
// Only tickets of the consent module edit this file (BE-001 R4).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type consentWiring struct{}

func (w *consentWiring) build(c *container) error { return nil }

func (w *consentWiring) tasks(c *container) {}

func (w *consentWiring) routes(c *container, api huma.API) {}
