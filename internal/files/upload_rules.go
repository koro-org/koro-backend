package files

import (
	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx/op"
)

// ExtUploadRules is the createUpload extension that publishes the browser
// allow-list of every uploadable purpose (BE-123): clients pre-check the
// chosen file with it instead of copying the purposeRule table. Go still
// checks every upload (checkUpload) and the content bytes on completion.
const ExtUploadRules = "x-koro-upload-rules"

// Org rules of an upload rule (createUpload.org_id).
const (
	UploadOrgRequired = "required"
	UploadOrgOptional = "optional"
	UploadOrgNone     = "none"
)

// UploadRule is the published allow-list row of one uploadable purpose.
type UploadRule struct {
	// MIMETypes are the content types the purpose accepts.
	MIMETypes []string `json:"mime_types"`
	// MaxBytes is the largest size_bytes of the purpose.
	MaxBytes int64 `json:"max_bytes"`
	// OrgID says whether createUpload needs org_id (required), accepts it
	// (optional) or refuses it (none).
	OrgID string `json:"org_id"`
}

// UploadRules returns the rule of every purpose browsers may upload, read
// from the same table checkUpload applies (one source: purposeRule).
func UploadRules() map[Purpose]UploadRule {
	out := make(map[Purpose]UploadRule, len(UploadPurposes()))
	for _, p := range UploadPurposes() {
		r := purposeRule[p]
		org := UploadOrgNone
		switch r.org {
		case orgRequired:
			org = UploadOrgRequired
		case orgOptional:
			org = UploadOrgOptional
		}
		out[p] = UploadRule{MIMETypes: append([]string(nil), r.mimes...), MaxBytes: r.maxBytes, OrgID: org}
	}
	return out
}

// uploadRulesExtension is the ExtUploadRules value: purpose → rule, keyed
// by the purpose string so the generated document is plain JSON.
func uploadRulesExtension() map[string]UploadRule {
	rules := UploadRules()
	out := make(map[string]UploadRule, len(rules))
	for p, r := range rules {
		out[string(p)] = r
	}
	return out
}

// createUpload request field types whose OpenAPI enum and bounds come from
// the constants checkUpload uses (BE-117 pattern: a struct tag cannot name
// a constant, so each type sets them in TransformSchema).
type (
	// uploadPurpose is createUpload.purpose (UploadPurposes).
	uploadPurpose string
	// uploadMIME is createUpload.content_type (UploadMIMEs).
	uploadMIME string
	// uploadSize is createUpload.size_bytes (1–MaxUploadBytes; the purpose
	// may allow less, ExtUploadRules).
	uploadSize int64
)

// TransformSchema implements huma.SchemaTransformer.
func (uploadPurpose) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Enum = op.Enum(UploadPurposes())
	return s
}

// TransformSchema implements huma.SchemaTransformer.
func (uploadMIME) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Enum = op.Enum(UploadMIMEs())
	return s
}

// TransformSchema implements huma.SchemaTransformer.
func (uploadSize) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	lo, hi := float64(1), float64(MaxUploadBytes)
	s.Minimum, s.Maximum = &lo, &hi
	return s
}
