package main

import "github.com/danielgtaylor/huma/v2"

// ledgerWiring wires the ledger module (L11; 01 §4.19). First owner: BE-039.
// Only tickets of the ledger module edit this file (BE-001 R4).
// Appends to hooks.openCommerce (BE-044).
//
// Stub: nothing is built, no task is collected and no route is registered
// until the module ticket fills it in (pattern: cmd/koro/wire.go).
type ledgerWiring struct{}

func (w *ledgerWiring) build(c *container) error { return nil }

func (w *ledgerWiring) tasks(c *container) {}

func (w *ledgerWiring) routes(c *container, api huma.API) {}
