package directory

import (
	"github.com/danielgtaylor/huma/v2"
)

// socialLinksMap is clubs.social_links in the contract (updateClub body and
// ClubDTO): its OpenAPI bounds come from the constants cleanSocialLinks
// checks with (BE-117 pattern, BE-127): at most maxSocial networks, each
// name matching socialKeyRe. huma has no propertyNames keyword of its own,
// so the JSON Schema keyword is written as an extension; the service still
// checks every name and URL.
type socialLinksMap map[string]string

// TransformSchema implements huma.SchemaTransformer.
func (socialLinksMap) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	n := maxSocial
	s.MaxProperties = &n
	if s.Extensions == nil {
		s.Extensions = map[string]any{}
	}
	s.Extensions["propertyNames"] = map[string]any{"pattern": socialKeyRe.String()}
	return s
}
