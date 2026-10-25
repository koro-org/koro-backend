package op

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
)

// Register registers o under the OpenAPI tag of its module: it sets
// o.Tags to [tag], adds the Koro extensions (Koro) and calls huma.Register.
// Every module registers its operations with it instead of a private reg
// helper (BE-111). Go has no generic methods, so the tag is an argument
// rather than a bound registrar value.
func Register[I, O any](api huma.API, tag string, o huma.Operation, m Meta, fn func(context.Context, *I) (*O, error)) {
	o.Tags = []string{tag}
	huma.Register(api, Koro(o, m), fn)
}
