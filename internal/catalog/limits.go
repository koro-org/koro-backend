package catalog

import (
	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx/op"
)

// TransformSchema implements huma.SchemaTransformer: blocked_sponsor_flags
// items are an enum of SponsorFlags in the contract (BE-127).
func (SponsorFlag) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Enum = op.Enum(SponsorFlags())
	return s
}
