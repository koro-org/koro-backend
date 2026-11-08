// Package outreachtest holds the shared test doubles of outreach's ports
// for the tests of other packages (BE-111: one double, no copies).
package outreachtest

import "koro/internal/outreach"

// NoNoise is an outreach.Noise that adds no offset: the tests that pin an
// estimate read the plain bucket of a size. Production wires
// outreach.KeyedNoise (cmd/koro); outreach's own tests cover the offset.
type NoNoise struct{}

var _ outreach.Noise = NoNoise{}

// Offset implements outreach.Noise.
func (NoNoise) Offset([]byte, int) (int, error) { return 0, nil }
