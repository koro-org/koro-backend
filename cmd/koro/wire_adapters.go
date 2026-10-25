package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Adapter selection (BE-113; config/provider.go). Every external port has
// one selection map in the wiring of its module: the adapter name of the
// port's configuration key (NOTIFY_EMAIL_PROVIDER, STORAGE_PROVIDER,
// FILES_SCANNER, NOTIFY_PUSH_PROVIDER) → the constructor of that adapter.
// Adding an adapter is a new file next to the port, its name in the config
// section and one line in the map; the port's contract suite proves it
// behaves like the others. TestWire_EveryConfiguredAdapterIsWired keeps the
// maps and the accepted names in step.
type adapters[T any] map[string]func() (T, error)

// pick builds the adapter named by the configuration key. An unknown name is
// an error (config validation normally stops it first).
func (a adapters[T]) pick(key, name string) (T, error) {
	build, ok := a[name]
	if !ok {
		var zero T
		return zero, fmt.Errorf("%s=%q: no such adapter (wired: %s)", key, name, strings.Join(a.names(), ", "))
	}
	v, err := build()
	if err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}

// names returns the wired adapter names, sorted.
func (a adapters[T]) names() []string { return slices.Sorted(maps.Keys(a)) }
