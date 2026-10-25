package identity

import "github.com/danielgtaylor/huma/v2"

// Request field types whose OpenAPI length bounds come from the constants
// the service checks with (BE-117): a struct tag cannot name a constant, so
// each type sets its bounds in TransformSchema and the generated contract
// (and the client Zod schemas built from it) can never drift from the Go
// rule again. huma counts characters as the service does; the service
// still checks after normalization (05 §9.5, NFKC for passwords) and keeps
// its own error codes for everything inside the bounds.
type (
	// nameText is a first or last name (cleanName: 1–maxNameLen).
	nameText string
	// emailText is an address the service parses (parseEmail: ≤ maxEmailLen).
	emailText string
	// signupPassword is the password of a new account, which has no MFA
	// yet (checkPassword: minPasswordLen–maxPasswordLen).
	signupPassword string
	// newPassword replaces the password of an account that may have MFA
	// (checkPassword: minPasswordLenMFA–maxPasswordLen).
	newPassword string
	// reasonText is a staff reason (cleanReason: minReasonLen–maxReasonLen).
	reasonText string
	// reviewNoteText is an optional review note (cleanReviewNote: ≤ maxReviewNote).
	reviewNoteText string
)

// lengthBounds sets the character bounds of a string schema; minLen 0
// leaves the minimum unset.
func lengthBounds(s *huma.Schema, minLen, maxLen int) *huma.Schema {
	if minLen > 0 {
		s.MinLength = &minLen
	}
	s.MaxLength = &maxLen
	return s
}

// TransformSchema implements huma.SchemaTransformer.
func (nameText) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return lengthBounds(s, 1, maxNameLen)
}

// TransformSchema implements huma.SchemaTransformer.
func (emailText) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return lengthBounds(s, 0, maxEmailLen)
}

// TransformSchema implements huma.SchemaTransformer.
func (signupPassword) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return lengthBounds(s, minPasswordLen, maxPasswordLen)
}

// TransformSchema implements huma.SchemaTransformer.
func (newPassword) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return lengthBounds(s, minPasswordLenMFA, maxPasswordLen)
}

// TransformSchema implements huma.SchemaTransformer.
func (reasonText) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return lengthBounds(s, minReasonLen, maxReasonLen)
}

// TransformSchema implements huma.SchemaTransformer.
func (reviewNoteText) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return lengthBounds(s, 0, maxReviewNote)
}
