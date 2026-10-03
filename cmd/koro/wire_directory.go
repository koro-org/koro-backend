package main

import "github.com/danielgtaylor/huma/v2"

// directoryWiring wires the directory module (L6; 01 §4.8). First owner: BE-020.
// Only tickets of the directory module edit this file (BE-001 R4).
// Provides hooks.domainResolver and hooks.clubInfo (BE-020).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type directoryWiring struct{}

func (w *directoryWiring) build(c *container) error { return nil }

func (w *directoryWiring) tasks(c *container) {}

func (w *directoryWiring) routes(c *container, api huma.API) {}
