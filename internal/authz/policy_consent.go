package authz

// consentPolicy is the policy of the consent module (03 §5.5). Only tickets of
// the consent module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every consent action (default deny) until the
// module ticket writes Visible and Grants.
func consentPolicy() Policy { return Policy{} }
